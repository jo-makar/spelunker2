package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/smtp"
	"path"
	"runtime"
)

func SendMail(body, subject string) error {
	var mailInfo struct {
		Username  string `json:"username"`
		Password  string `json:"password"`
		Sender    string `json:"sendor"`
		Recipient string `json:"recipient"`
		SmtpHost  string `json:"smtphost"`
		SmtpPort  int    `json:"smtpport"`
	}

	var jsonpath string
	if _, file, _, ok := runtime.Caller(1); !ok {
		return errors.New("runtime.Caller() failed")
	} else {
		jsonpath = path.Join(path.Dir(file), "mail.json")
	}

	if data, err := ioutil.ReadFile(jsonpath); err != nil {
		return err
	} else {
		if err := json.Unmarshal(data, &mailInfo); err != nil {
			return err
		}
	}

	var msg bytes.Buffer

	fmt.Fprintf(&msg, "To: %s\r\n", mailInfo.Recipient)
	fmt.Fprintf(&msg, "Subject: %s\r\n", subject)

	fmt.Fprint(&msg, "MIME-version: 1.0\r\n")
	fmt.Fprint(&msg, `Content-Type: text/html; charset="UTF-8"` + "\r\n")

	fmt.Fprint(&msg, "\r\n")

	fmt.Fprint(&msg, body)

	if err := smtp.SendMail(
			fmt.Sprintf("%s:%d", mailInfo.SmtpHost, mailInfo.SmtpPort),
			smtp.PlainAuth("", mailInfo.Username, mailInfo.Password, mailInfo.SmtpHost),
			mailInfo.Sender,
			[]string{mailInfo.Recipient},
			msg.Bytes()); err != nil {
		return err
	}

	return nil
}
