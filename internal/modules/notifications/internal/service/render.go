// Responsibility: deterministic templates and safe frozen envelopes.
// Dependencies: stdlib HTML templates; worker service calls Render before send.
// Invariants: canonical recipients; HTML escapes data and headers reject CR/LF.
package service

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"html/template"
	"net/mail"
	"strings"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
)

func ValidRecipient(s string) bool {
	if len(s) > 254 || strings.ContainsAny(s, "\r\n") {
		return false
	}
	a, e := mail.ParseAddress(s)
	if e != nil || a.Address != s {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	if at < 1 {
		return false
	}
	return len(s[at+1:]) > 0
}
func Render(event, recipient, locale, key, reference string) (contract.Message, error) {
	if event == "" || len(event) > 64 || !ValidRecipient(recipient) || len(reference) > 256 || strings.ContainsAny(reference, "\r\n") {
		return contract.Message{}, fmt.Errorf("NOTIFICATION_INVALID_ENVELOPE")
	}
	english := map[string]string{"order.paid": "Payment received", "order.shipped": "Order shipped", "order.refund": "Refund processed", "quote.sent": "Quote ready", "quote.approved": "Quote approved", "quote.converted": "Quote converted to order"}
	chinese := map[string]string{"order.paid": "付款已收到", "order.shipped": "訂單已出貨", "order.refund": "退款已處理", "quote.sent": "報價已準備", "quote.approved": "報價已核准", "quote.converted": "報價已轉為訂單"}
	subject, ok := english[key]
	if !ok {
		return contract.Message{}, fmt.Errorf("NOTIFICATION_UNKNOWN_TEMPLATE")
	}
	label := "Reference"
	if locale == "zh" || locale == "zh-TW" {
		subject = chinese[key]
		label = "參考編號"
	} else if locale != "en" {
		return contract.Message{}, fmt.Errorf("NOTIFICATION_UNKNOWN_LOCALE")
	}
	var html bytes.Buffer
	t := template.Must(template.New("notice").Parse(`<html><body><h1>{{.Subject}}</h1><p>{{.Label}}: {{.Reference}}</p></body></html>`))
	if e := t.Execute(&html, struct{ Subject, Label, Reference string }{subject, label, reference}); e != nil {
		return contract.Message{}, e
	}
	hash := sha256.Sum256([]byte(event + "\x00" + strings.ToLower(recipient) + "\x00" + key))
	return contract.Message{Key: fmt.Sprintf("%x", hash), Recipient: recipient, Subject: subject, Text: subject + "\n" + label + ": " + reference + "\n", HTML: html.String()}, nil
}
