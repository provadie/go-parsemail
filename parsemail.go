package parsemail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"path/filepath"
	"strings"
	"time"
)

const contentTypeMultipartSigned = "multipart/signed"
const contentTypeMultipartMixed = "multipart/mixed"
const contentTypeMultipartAlternative = "multipart/alternative"
const contentTypeMultipartRelated = "multipart/related"
const contentTypeTextCalendar = "text/calendar"
const contentTypeTextHtml = "text/html"
const contentTypeTextPlain = "text/plain"
const contentTypeTextExtension = "text/x-"
const contentTypeApplicationOctetStream = "application/octet-stream"
const maxDepthOfMultipartMixed = 3

type MailParser interface {
	Parse(r io.Reader) (email Email, err error)
}

type mailParser struct {
	word    *mime.WordDecoder
	address *mail.AddressParser

	decodeQuotedNames bool
}

// NewParserOptions specifies options for creating a new mail parser instance.
type NewParserOptions struct {
	// WordDecoder is used to decode RFC 2047 encoded-words in headers.
	// If nil, a default mime.WordDecoder will be used. Can be used to decode other character sets.
	WordDecoder *mime.WordDecoder

	// DecodeQuotedNames indicates whether to decode faulty formatted encoded names in email addresses.
	// Sometimes the display names in email addresses are not properly formatted, and this option
	// allows the parser to attempt to decode them. Defaults to false.
	// If set to true, it will try to decode names like `"=?UTF-8?Q?Peter_Pahol=C3=ADk?=" <peter.paholik@gmail.com>`.
	DecodeQuotedNames bool
}

// NewParser constructs a new mail parser instance with the provided options.
func NewParser(options *NewParserOptions) MailParser {
	if options == nil {
		options = &NewParserOptions{}
	}
	if options.WordDecoder == nil {
		options.WordDecoder = &mime.WordDecoder{}
	}
	return mailParser{
		word:    options.WordDecoder,
		address: &mail.AddressParser{WordDecoder: options.WordDecoder},
		
		decodeQuotedNames: options.DecodeQuotedNames,
	}
}

// Parse an email message read from io.Reader into parsemail.Email struct
func Parse(r io.Reader) (email Email, err error) {
	parser := NewParser(nil)
	return parser.Parse(r)
}

// Parse an email message read from io.Reader into parsemail.Email struct
func (parser mailParser) Parse(r io.Reader) (email Email, err error) {
	msg, err := mail.ReadMessage(r)
	if err != nil {
		return
	}

	email, err = parser.createEmailFromHeader(msg.Header)
	if err != nil {
		return
	}

	email.ContentType = msg.Header.Get("Content-Type")
	contentType, params, err := parser.parseContentType(email.ContentType)
	if err != nil {
		return
	}

	switch contentType {
	case contentTypeMultipartSigned:
		email.TextBody, email.HTMLBody, email.Attachments, email.EmbeddedFiles, email.TextBodies, email.HTMLBodies, err = parser.parseMultipartMixed(msg.Body, params["boundary"], 1)
	case contentTypeMultipartMixed:
		email.TextBody, email.HTMLBody, email.Attachments, email.EmbeddedFiles, email.TextBodies, email.HTMLBodies, err = parser.parseMultipartMixed(msg.Body, params["boundary"], 1)
	case contentTypeMultipartAlternative:
		email.TextBody, email.HTMLBody, email.Attachments, email.EmbeddedFiles, email.TextBodies, email.HTMLBodies, err = parser.parseMultipartAlternative(msg.Body, params["boundary"])
	case contentTypeMultipartRelated:
		email.TextBody, email.HTMLBody, email.Attachments, email.EmbeddedFiles, email.TextBodies, email.HTMLBodies, err = parser.parseMultipartRelated(msg.Body, params["boundary"])
	case contentTypeTextPlain:
		buf := new(bytes.Buffer)
		tee := io.TeeReader(msg.Body, buf)
		var message []byte
		message, err = io.ReadAll(tee)
		if err != nil {
			return
		}
		email.TextBody = strings.TrimSuffix(string(message[:]), "\n")
		var data io.Reader
		data, err = decodeContent(buf, email.Header.Get("Content-Transfer-Encoding"))
		if err != nil {
			return
		}
		email.TextBodies = []*TextBody{
			{
				Body{
					ContentType: contentType,
					Params:      params,
					Data:        data,
				},
			},
		}
	case contentTypeTextHtml:
		buf := new(bytes.Buffer)
		tee := io.TeeReader(msg.Body, buf)
		var message []byte
		message, err = io.ReadAll(tee)
		if err != nil {
			return
		}
		email.HTMLBody = strings.TrimSuffix(string(message[:]), "\n")
		var data io.Reader
		data, err = decodeContent(buf, email.Header.Get("Content-Transfer-Encoding"))
		if err != nil {
			return
		}
		email.HTMLBodies = []*HTMLBody{
			{
				Body{
					ContentType: contentType,
					Params:      params,
					Data:        data,
				},
			},
		}
	default:
		email.Content, err = decodeContent(msg.Body, msg.Header.Get("Content-Transfer-Encoding"))
	}

	return
}

func (parser mailParser) createEmailFromHeader(header mail.Header) (email Email, err error) {
	hp := headerParser{header: &header, parser: parser}

	email.Subject = hp.parseHeader(header.Get("Subject"))
	email.From = hp.parseAddressList(header.Get("From"))
	email.Sender = hp.parseAddress(header.Get("Sender"))
	email.ReplyTo = hp.parseAddressList(header.Get("Reply-To"))
	email.To = hp.parseAddressList(header.Get("To"))
	email.Cc = hp.parseAddressList(header.Get("Cc"))
	email.Bcc = hp.parseAddressList(header.Get("Bcc"))
	email.DeliveredTo = hp.parseAddressValues(header["Delivered-To"])
	email.Date = hp.parseTime(header.Get("Date"))
	email.ResentFrom = hp.parseAddressList(header.Get("Resent-From"))
	email.ResentSender = hp.parseAddress(header.Get("Resent-Sender"))
	email.ResentTo = hp.parseAddressList(header.Get("Resent-To"))
	email.ResentCc = hp.parseAddressList(header.Get("Resent-Cc"))
	email.ResentBcc = hp.parseAddressList(header.Get("Resent-Bcc"))
	email.ResentMessageID = hp.parseMessageId(header.Get("Resent-Message-ID"))
	email.MessageID = hp.parseMessageId(header.Get("Message-ID"))
	email.InReplyTo = hp.parseMessageIdList(header.Get("In-Reply-To"))
	email.References = hp.parseMessageIdList(header.Get("References"))
	email.ResentDate = hp.parseTime(header.Get("Resent-Date"))

	if hp.err != nil {
		err = hp.err
		return
	}

	//decode whole header for easier access to extra fields
	//todo: should we decode? aren't only standard fields mime encoded?
	email.Header, err = parser.decodeHeaderMime(header)
	if err != nil {
		return
	}

	return
}

func (parser mailParser) parseContentType(contentTypeHeader string) (contentType string, params map[string]string, err error) {
	if contentTypeHeader == "" {
		contentType = contentTypeTextPlain
		return
	}

	return mime.ParseMediaType(contentTypeHeader)
}

func (parser mailParser) parseMultipartRelated(msg io.Reader, boundary string) (textBody, htmlBody string, attachments []Attachment, embeddedFiles []EmbeddedFile, textBodies []*TextBody, htmlBodies []*HTMLBody, err error) {
	pmr := multipart.NewReader(msg, boundary)
	for {
		part, err := NextPart(pmr)

		if err == io.EOF {
			break
		} else if err != nil {
			return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
		}

		contentType, params := part.contentType, part.contentTypeParams
		if err != nil {
			return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
		}

		switch contentType {
		case contentTypeTextPlain:
			ppContent, err := io.ReadAll(part.tee)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			textBody += strings.TrimSuffix(string(ppContent[:]), "\n")
			b, err := part.newBody()
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			textBodies = append(textBodies, &TextBody{
				Body: *b,
			})
		case contentTypeTextHtml:
			ppContent, err := io.ReadAll(part.tee)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}

			htmlBody += strings.TrimSuffix(string(ppContent[:]), "\n")
			b, err := part.newBody()
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			htmlBodies = append(htmlBodies, &HTMLBody{
				Body: *b,
			})
		case contentTypeTextCalendar:
			ef, err := parser.decodeEmbeddedFile(part)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			embeddedFiles = append(embeddedFiles, ef)
		case contentTypeMultipartAlternative:
			tb, hb, af, ef, tbs, hbs, err := parser.parseMultipartAlternative(part, params["boundary"])
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			htmlBody += hb
			textBody += tb
			attachments = append(attachments, af...)
			embeddedFiles = append(embeddedFiles, ef...)
			textBodies = append(textBodies, tbs...)
			htmlBodies = append(htmlBodies, hbs...)
		default:
			if isEmbeddedFile(part) {
				ef, err := parser.decodeEmbeddedFile(part)
				if err != nil {
					return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
				}

				embeddedFiles = append(embeddedFiles, ef)
			} else {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, fmt.Errorf("Can't process multipart/related inner mime type: %s", contentType)
			}
		}
	}

	return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
}

func (parser mailParser) parseMultipartAlternative(msg io.Reader, boundary string) (textBody, htmlBody string, attachments []Attachment, embeddedFiles []EmbeddedFile, textBodies []*TextBody, htmlBodies []*HTMLBody, err error) {
	pmr := multipart.NewReader(msg, boundary)
	for {
		part, err := NextPart(pmr)

		if err == io.EOF {
			break
		} else if err != nil {
			return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
		}

		contentType, params := part.contentType, part.contentTypeParams
		if err != nil {
			return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
		}

		switch contentType {
		case contentTypeTextPlain:
			ppContent, err := io.ReadAll(part.tee)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			textBody += strings.TrimSuffix(string(ppContent[:]), "\n")
			b, err := part.newBody()
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			textBodies = append(textBodies, &TextBody{
				Body: *b,
			})
		case contentTypeTextHtml:
			ppContent, err := io.ReadAll(part.tee)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			htmlBody += strings.TrimSuffix(string(ppContent[:]), "\n")
			b, err := part.newBody()
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			htmlBodies = append(htmlBodies, &HTMLBody{
				Body: *b,
			})
		case contentTypeTextCalendar:
			ef, err := parser.decodeEmbeddedFile(part)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			embeddedFiles = append(embeddedFiles, ef)
		case contentTypeMultipartRelated:
			tb, hb, af, ef, tbs, hbs, err := parser.parseMultipartRelated(part, params["boundary"])
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			htmlBody += hb
			textBody += tb
			attachments = append(attachments, af...)
			embeddedFiles = append(embeddedFiles, ef...)
			textBodies = append(textBodies, tbs...)
			htmlBodies = append(htmlBodies, hbs...)
		case contentTypeMultipartMixed:
			tb, hb, at, ef, tbs, hbs, err := parser.parseMultipartMixed(part, params["boundary"], 1)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			htmlBody += hb
			textBody += tb
			attachments = append(attachments, at...)
			embeddedFiles = append(embeddedFiles, ef...)
			textBodies = append(textBodies, tbs...)
			htmlBodies = append(htmlBodies, hbs...)
		default:
			if strings.HasPrefix(contentType, contentTypeTextExtension) {
				continue
			}
			if isEmbeddedFile(part) {
				ef, err := parser.decodeEmbeddedFile(part)
				if err != nil {
					return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
				}

				embeddedFiles = append(embeddedFiles, ef)
			} else {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, fmt.Errorf("Can't process multipart/alternative inner mime type: %s", contentType)
			}
		}
	}

	return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
}

func (parser mailParser) parseMultipartMixed(msg io.Reader, boundary string, depth int) (textBody, htmlBody string, attachments []Attachment, embeddedFiles []EmbeddedFile, textBodies []*TextBody, htmlBodies []*HTMLBody, err error) {
	if depth > maxDepthOfMultipartMixed {
		return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, fmt.Errorf("nested multiple/mixed above max depth")
	}
	mr := multipart.NewReader(msg, boundary)
	for {
		part, err := NextPart(mr)
		if err == io.EOF {
			break
		} else if err != nil {
			return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
		}
		if isAttachment(part) {
			at, err := parser.decodeAttachment(part)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			attachments = append(attachments, at)
			continue
		}
		contentType, params := part.contentType, part.contentTypeParams
		if err != nil {
			return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
		}
		if contentType == contentTypeMultipartAlternative {
			tb, hb, ats, efs, tbs, hbs, err := parser.parseMultipartAlternative(part, params["boundary"])
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			textBody += tb
			htmlBody += hb
			attachments = append(attachments, ats...)
			embeddedFiles = append(embeddedFiles, efs...)
			textBodies = append(textBodies, tbs...)
			htmlBodies = append(htmlBodies, hbs...)
		} else if contentType == contentTypeMultipartRelated {
			tb, hb, ats, efs, tbs, hbs, err := parser.parseMultipartRelated(part, params["boundary"])
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			textBody += tb
			htmlBody += hb
			attachments = append(attachments, ats...)
			embeddedFiles = append(embeddedFiles, efs...)
			textBodies = append(textBodies, tbs...)
			htmlBodies = append(htmlBodies, hbs...)
		} else if contentType == contentTypeMultipartMixed {
			tb, hb, ats, efs, tbs, hbs, err := parser.parseMultipartMixed(part, params["boundary"], depth+1)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			textBody += tb
			hb += hb
			attachments = append(attachments, ats...)
			embeddedFiles = append(embeddedFiles, efs...)
			textBodies = append(textBodies, tbs...)
			htmlBodies = append(htmlBodies, hbs...)
		} else if contentType == contentTypeTextPlain {
			ppContent, err := io.ReadAll(part.tee)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			textBody += strings.TrimSuffix(string(ppContent[:]), "\n")
			b, err := part.newBody()
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			textBodies = append(textBodies, &TextBody{
				Body: *b,
			})
		} else if contentType == contentTypeTextHtml {
			ppContent, err := io.ReadAll(part.tee)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			htmlBody += strings.TrimSuffix(string(ppContent[:]), "\n")
			b, err := part.newBody()
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			htmlBodies = append(htmlBodies, &HTMLBody{
				Body: *b,
			})
		} else if contentType == contentTypeTextCalendar {
			ef, err := parser.decodeEmbeddedFile(part)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			embeddedFiles = append(embeddedFiles, ef)
		} else if contentType == contentTypeApplicationOctetStream {
			at, err := parser.decodeAttachment(part)
			if err != nil {
				return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
			}
			if at.Filename == "" {
				if name, ok := params["name"]; ok {
					at.Filename, err = parser.decodeMimeSentence(name)
					if err != nil {
						return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
					}
				}
			}
			attachments = append(attachments, at)
		} else {
			return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, fmt.Errorf("Unknown multipart/mixed nested mime type: %s", contentType)
		}
	}

	return textBody, htmlBody, attachments, embeddedFiles, textBodies, htmlBodies, err
}

// decodeMimeSentence decodes all encoded-words of the given string.
func (parser mailParser) decodeMimeSentence(s string) (string, error) {
	return parser.word.DecodeHeader(s)
}

// decodeHeaderMime decodes all encoded-words values of the header map
func (parser mailParser) decodeHeaderMime(header mail.Header) (mail.Header, error) {
	parsedHeader := make(mail.Header, len(header))

	for headerName, headerData := range header {

		parsedHeaderData := make([]string, 0, len(headerData))
		for _, headerValue := range headerData {
			decodedHeader, err := parser.decodeMimeSentence(headerValue)
			if err != nil {
				return nil, err
			}
			parsedHeaderData = append(parsedHeaderData, decodedHeader)
		}

		parsedHeader[headerName] = parsedHeaderData
	}

	return parsedHeader, nil
}

func isEmbeddedFile(part *Part) bool {
	return part.contentTransferEncoding != ""
}

func (parser mailParser) decodeEmbeddedFile(part *Part) (ef EmbeddedFile, err error) {
	cid, err := parser.decodeMimeSentence(part.Header.Get("Content-Id"))
	if err != nil {
		return
	}
	decoded, err := decodeContent(part, part.contentTransferEncoding)
	if err != nil {
		return
	}

	ef.CID = strings.Trim(cid, "<>")
	ef.Data = decoded
	ef.ContentType = part.contentType

	if name, ok := part.contentTypeParams["name"]; ok {
		name = filepath.Base(name)
		ef.Filename, err = parser.decodeMimeSentence(name)
		if err != nil {
			return
		}
	}

	return
}

func isAttachment(part *Part) bool {
	return part.FileName() != "" || strings.ToLower(part.contentDisposition) == "attachment"
}

func (parser mailParser) decodeAttachment(part *Part) (at Attachment, err error) {
	filename, err := parser.decodeMimeSentence(part.FileName())
	if err != nil {
		return
	}
	if filename == "" {
		if name, ok := part.contentTypeParams["name"]; ok {
			filename, err = parser.decodeMimeSentence(name)
			if err != nil {
				return
			}
		}
	}
	decoded, err := decodeContent(part, part.Header.Get("Content-Transfer-Encoding"))
	if err != nil {
		return
	}

	at.Filename = filename
	at.Data = decoded

	at.ContentType, _, err = mime.ParseMediaType(part.Header.Get("Content-Type"))
	if err != nil {
		return
	}

	return
}

func decodeContent(content io.Reader, encoding string) (io.Reader, error) {
	switch strings.ToLower(encoding) {
	case "quoted-printable":
		r := quotedprintable.NewReader(content)
		out := new(bytes.Buffer)
		_, err := io.Copy(out, r)
		if err != nil {
			return nil, err
		}
		return out, nil
	case "base64":
		decoded := base64.NewDecoder(base64.StdEncoding, content)
		b, err := io.ReadAll(decoded)
		if err != nil {
			return nil, err
		}

		return bytes.NewReader(b), nil
	case "7bit", "8bit", "":
		dd, err := io.ReadAll(content)
		if err != nil {
			return nil, err
		}

		return bytes.NewReader(dd), nil
	default:
		return nil, fmt.Errorf("unknown encoding: %s", encoding)
	}
}

type headerParser struct {
	parser mailParser
	header *mail.Header
	err    error
}

func (hp *headerParser) parseHeader(s string) (result string) {
	if hp.err != nil {
		return ""
	}

	result, hp.err = hp.parser.decodeMimeSentence(s)
	return result
}

func (hp *headerParser) parseAddress(s string) (ma *mail.Address) {
	if hp.err != nil {
		return nil
	}

	if strings.Trim(s, " \n") != "" {
		ma, hp.err = hp.parser.address.Parse(s)
		if hp.parser.decodeQuotedNames {
			hp.decodeAddressName(ma)
		}

		return ma
	}

	return nil
}

func (hp *headerParser) parseAddressList(s string) (ma []*mail.Address) {
	if hp.err != nil {
		return
	}

	if strings.Trim(s, " \n") != "" {
		ma, hp.err = hp.parser.address.ParseList(s)
		if hp.parser.decodeQuotedNames {
			hp.decodeAddressNames(ma)
		}
		return
	}

	return
}

func (hp *headerParser) decodeAddressName(addr *mail.Address) {
	if addr == nil || hp.err != nil {
		return
	}
	newName, decodeErr := hp.parser.word.Decode(addr.Name)
	if decodeErr == nil {
		addr.Name = newName
	} else if !strings.Contains(decodeErr.Error(), "mime: invalid RFC 2047 encoded-word") { // !errors.Is(decodeErr, mime.errInvalidWord) {
		hp.err = decodeErr
	}
}

func (hp *headerParser) decodeAddressNames(list []*mail.Address) {
	if hp.err != nil {
		return
	}
	for _, addr := range list {
		hp.decodeAddressName(addr)
	}
}

func (hp *headerParser) parseAddressValues(s []string) (ma []*mail.Address) {
	for _, s := range s {
		var result *mail.Address
		result = hp.parseAddress(s)
		if hp.err != nil {
			return
		}
		ma = append(ma, result)
	}

	return
}

func (hp *headerParser) parseTime(s string) (t time.Time) {
	if hp.err != nil || s == "" {
		return
	}

	formats := []string{
		time.RFC1123,
		time.RFC1123Z,
		"Mon, 2 Jan 2006 15:04:05 -0700",
		time.RFC1123Z + " (MST)",
		"Mon, 2 Jan 2006 15:04:05 -0700 (MST)",
		time.RFC1123Z + " (GMT-07:00)", // include additional tz
		"Mon, 2 Jan 2006 15:04:05 -0700 (GMT-07:00)", // include additional tz
		time.RFC1123[5:], // omit dow
		time.RFC1123Z[5:], // omit dow
		"2 Jan 2006 15:04:05 -0700",
		time.RFC1123Z[5:] + " (MST)", // omit dow
		"2 Jan 2006 15:04:05 -0700 (MST)",
		time.RFC1123Z[5:] + " (GMT-07:00)", // include additional tz and omit dow
		"2 Jan 2006 15:04:05 -0700 (GMT-07:00)", // include additional tz
	}

	for _, format := range formats {
		t, hp.err = time.Parse(format, s)
		if hp.err == nil {
			return
		}
	}

	return
}

func (hp *headerParser) parseMessageId(s string) string {
	if hp.err != nil {
		return ""
	}

	return strings.Trim(s, "<> ")
}

func (hp *headerParser) parseMessageIdList(s string) (result []string) {
	if hp.err != nil {
		return
	}

	for _, p := range strings.Split(s, " ") {
		if strings.Trim(p, " \n") != "" {
			result = append(result, hp.parseMessageId(p))
		}
	}

	return
}

// Attachment with filename, content type and data (as a io.Reader)
type Attachment struct {
	Filename    string
	ContentType string
	Data        io.Reader
}

// EmbeddedFile with content id, content type and data (as a io.Reader)
type EmbeddedFile struct {
	CID         string
	Filename    string
	ContentType string
	Data        io.Reader
}

// Email with fields for all the headers defined in RFC5322 with it's attachments and
type Email struct {
	Header mail.Header

	Subject     string
	Sender      *mail.Address
	From        []*mail.Address
	ReplyTo     []*mail.Address
	To          []*mail.Address
	Cc          []*mail.Address
	Bcc         []*mail.Address
	DeliveredTo []*mail.Address
	Date        time.Time
	MessageID   string
	InReplyTo   []string
	References  []string

	ResentFrom      []*mail.Address
	ResentSender    *mail.Address
	ResentTo        []*mail.Address
	ResentDate      time.Time
	ResentCc        []*mail.Address
	ResentBcc       []*mail.Address
	ResentMessageID string

	ContentType string
	Content     io.Reader

	HTMLBody string
	TextBody string

	Attachments   []Attachment
	EmbeddedFiles []EmbeddedFile

	HTMLBodies []*HTMLBody
	TextBodies []*TextBody
}

type Body struct {
	ContentType string
	Params      map[string]string
	Data        io.Reader
}

type HTMLBody struct {
	Body
}

type TextBody struct {
	Body
}

type Part struct {
	*multipart.Part
	contentType              string
	contentTypeParams        map[string]string
	contentDisposition       string
	contentDispositionParams map[string]string
	contentTransferEncoding  string
	tee                      io.Reader
	out                      *bytes.Buffer
}

func NextPart(r *multipart.Reader) (*Part, error) {
	p, err := r.NextPart()
	if err != nil {
		return nil, err
	}
	return newPart(p)
}

func newPart(part *multipart.Part) (out *Part, err error) {
	out = &Part{
		Part: part,
	}
	out.contentType, out.contentTypeParams, err = mime.ParseMediaType(part.Header.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	if part.Header.Get("Content-Disposition") != "" {
		out.contentDisposition, out.contentDispositionParams, err = mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			return nil, err
		}
	}
	out.contentTransferEncoding = part.Header.Get("Content-Transfer-Encoding")
	out.out = new(bytes.Buffer)
	out.tee = io.TeeReader(part, out.out)
	return out, nil
}

func (p *Part) newBody() (*Body, error) {
	data, err := decodeContent(p.out, p.contentTransferEncoding)
	if err != nil {
		return nil, err
	}
	return &Body{
		ContentType: p.contentType,
		Params:      p.contentTypeParams,
		Data:        data,
	}, nil
}

func (p *Part) FileName() string {
	if p.contentDispositionParams != nil {
		return p.contentDispositionParams["filename"]
	}

	return ""
}
