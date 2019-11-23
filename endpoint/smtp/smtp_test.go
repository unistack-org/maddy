package smtp

import (
	"flag"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"github.com/foxcpp/go-mockdns"
	"github.com/foxcpp/maddy/config"
	"github.com/foxcpp/maddy/exterrors"
	"github.com/foxcpp/maddy/module"
	"github.com/foxcpp/maddy/msgpipeline"
	"github.com/foxcpp/maddy/testutils"
)

var testPort string

const testMsg = "From: <sender@example.org>\r\n" +
	"Subject: Hello there!\r\n" +
	"\r\n" +
	"foobar\r\n"

func testEndpoint(t *testing.T, modName string, auth module.AuthProvider, tgt module.DeliveryTarget, checks []module.Check, cfg []config.Node) *Endpoint {
	t.Helper()

	mod, err := New(modName, []string{"tcp://127.0.0.1:" + testPort})
	if err != nil {
		t.Fatal(err)
	}
	endp := mod.(*Endpoint)

	endp.resolver = &mockdns.Resolver{
		Zones: map[string]mockdns.Zone{
			"mx.example.org.": {
				A: []string{"127.0.0.1"},
			},
			"1.0.0.127.in-addr.arpa.": {
				PTR: []string{"mx.example.org"},
			},
		},
	}
	endp.Log = testutils.Logger(t, "smtp")

	cfg = append(cfg,
		config.Node{
			Name: "hostname",
			Args: []string{"mx.example.com"},
		},
		config.Node{
			Name: "tls",
			Args: []string{"off"},
		},
		config.Node{ // To make it succeed, pipeline is actually replaced below.
			Name: "deliver_to",
			Args: []string{"dummy"},
		},
	)

	if auth != nil {
		cfg = append(cfg, config.Node{
			Name: "auth",
			Args: []string{"dummy"},
		})
	}

	err = endp.Init(config.NewMap(nil, &config.Node{
		Children: cfg,
	}))
	if err != nil {
		t.Fatal(err)
	}

	endp.Auth = auth

	endp.pipeline = msgpipeline.Mock(tgt, checks)
	endp.pipeline.Hostname = "mx.example.com"
	endp.pipeline.Resolver = endp.resolver
	endp.pipeline.Log = testutils.Logger(t, "smtp/pipeline")

	return endp
}

func submitMsg(t *testing.T, cl *smtp.Client, from string, rcpts []string, msg string) error {
	t.Helper()

	// Error for this one is ignore because it fails if EHLO was already sent
	// and submitMsg can happen multiple times.
	cl.Hello("mx.example.org")
	if err := cl.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range rcpts {
		if err := cl.Rcpt(rcpt); err != nil {
			return err
		}
	}
	data, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := data.Write([]byte(msg)); err != nil {
		return err
	}
	if err := data.Close(); err != nil {
		return err
	}

	return nil
}

func TestSMTPDelivery(t *testing.T) {
	tgt := testutils.Target{}
	endp := testEndpoint(t, "smtp", nil, &tgt, nil, nil)
	defer endp.Close()

	cl, err := smtp.Dial("127.0.0.1:" + testPort)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	err = submitMsg(t, cl, "sender@example.org", []string{"rcpt1@example.com", "rcpt2@example.com"}, testMsg)
	if err != nil {
		t.Fatal(err)
	}

	if len(tgt.Messages) != 1 {
		t.Fatal("Expected a message, got", len(tgt.Messages))
	}
	msg := tgt.Messages[0]
	msgID := testutils.CheckMsgID(t, &msg, "sender@example.org", []string{"rcpt1@example.com", "rcpt2@example.com"}, "")

	receivedPrefix := `from mx.example.org (mx.example.org [127.0.0.1]) by mx.example.com (envelope-sender <sender@example.org>) with ESMTP id ` + msgID

	if !strings.HasPrefix(msg.Header.Get("Received"), receivedPrefix) {
		t.Error("Wrong Received contents:", msg.Header.Get("Received"))
	}

	if msg.MsgMeta.SrcProto != "ESMTP" {
		t.Error("Wrong SrcProto:", msg.MsgMeta.SrcProto)
	}

	rdnsName, _ := msg.MsgMeta.SrcRDNSName.Get().(string)
	if rdnsName != "mx.example.org" {
		t.Error("Wrong rDNS name:", rdnsName)
	}
}

func TestSMTPDelivery_rDNSError(t *testing.T) {
	tgt := testutils.Target{}
	endp := testEndpoint(t, "smtp", nil, &tgt, nil, nil)
	defer endp.Close()

	endp.resolver.(*mockdns.Resolver).Zones["1.0.0.127.in-addr.arpa."] = mockdns.Zone{
		Err: &net.DNSError{
			Name:   "1.0.0.127.in-addr.arpa.",
			Server: "127.0.0.1:53",
			Err:    "bad",
		},
	}

	cl, err := smtp.Dial("127.0.0.1:" + testPort)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	err = submitMsg(t, cl, "sender@example.org", []string{"rcpt1@example.com", "rcpt2@example.com"}, testMsg)
	if err != nil {
		t.Fatal(err)
	}

	if len(tgt.Messages) != 1 {
		t.Fatal("Expected a message, got", len(tgt.Messages))
	}
	msg := tgt.Messages[0]
	testutils.CheckMsgID(t, &msg, "sender@example.org", []string{"rcpt1@example.com", "rcpt2@example.com"}, "")

	rdnsName := msg.MsgMeta.SrcRDNSName.Get()
	if rdnsName != nil {
		t.Errorf("Wrong rDNS name: %#+v", rdnsName)
	}
}

func TestSMTPDelivery_EarlyCheck_Fail(t *testing.T) {
	tgt := testutils.Target{}
	endp := testEndpoint(t, "smtp", nil, &tgt, []module.Check{
		&testutils.Check{
			EarlyErr: &exterrors.SMTPError{
				Code:    523,
				Message: "Hey",
			},
		},
	}, nil)
	defer endp.Close()

	cl, err := smtp.Dial("127.0.0.1:" + testPort)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	err = cl.Mail("sender@example.org")
	if err == nil {
		t.Fatal("Expected an error, got none")
	}

	smtpErr, ok := err.(*smtp.SMTPError)
	if !ok {
		t.Fatal("Non-SMTPError returned")
	}

	if smtpErr.Code != 523 {
		t.Fatal("Wrong SMTP code:", smtpErr.Code)
	}
	if smtpErr.Message != "Hey" {
		t.Fatal("Wrong SMTP message:", smtpErr.Message)
	}
}

func TestSMTPDeliver_CheckError(t *testing.T) {
	tgt := testutils.Target{}
	endp := testEndpoint(t, "smtp", nil, &tgt, []module.Check{
		&testutils.Check{
			ConnRes: module.CheckResult{
				Reason: &exterrors.SMTPError{
					Code:    523,
					Message: "Hey",
				},
				Reject: true,
			},
		},
	}, nil)
	endp.deferServerReject = false
	defer endp.Close()

	cl, err := smtp.Dial("127.0.0.1:" + testPort)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	err = cl.Mail("sender@example.org")
	if err == nil {
		t.Fatal("Expected an error, got none")
	}
	smtpErr, ok := err.(*smtp.SMTPError)
	if !ok {
		t.Fatal("Non-SMTPError returned")
	}

	if smtpErr.Code != 523 {
		t.Fatal("Wrong SMTP code:", smtpErr.Code)
	}
	if !strings.HasPrefix(smtpErr.Message, "Hey") {
		t.Fatal("Wrong SMTP message:", smtpErr.Message)
	}
}

func TestSMTPDeliver_CheckError_Deferred(t *testing.T) {
	tgt := testutils.Target{}
	endp := testEndpoint(t, "smtp", nil, &tgt, []module.Check{
		&testutils.Check{
			ConnRes: module.CheckResult{
				Reason: &exterrors.SMTPError{
					Code:    523,
					Message: "Hey",
				},
				Reject: true,
			},
		},
	}, nil)
	endp.deferServerReject = true
	defer endp.Close()

	cl, err := smtp.Dial("127.0.0.1:" + testPort)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	err = cl.Mail("sender@example.org")
	if err != nil {
		t.Fatal(err)
	}

	checkErr := func(err error) {
		if err == nil {
			t.Fatal("Expected an error, got none")
		}
		smtpErr, ok := err.(*smtp.SMTPError)
		if !ok {
			t.Fatal("Non-SMTPError returned")
		}

		if smtpErr.Code != 523 {
			t.Fatal("Wrong SMTP code:", smtpErr.Code)
		}
		if !strings.HasPrefix(smtpErr.Message, "Hey") {
			t.Fatal("Wrong SMTP message:", smtpErr.Message)
		}
	}

	checkErr(cl.Rcpt("test1@example.org"))
	checkErr(cl.Rcpt("test1@example.org"))
	checkErr(cl.Rcpt("test2@example.org"))
}

func TestSMTPDelivery_Multi(t *testing.T) {
	tgt := testutils.Target{}
	endp := testEndpoint(t, "smtp", nil, &tgt, nil, nil)
	defer endp.Close()

	cl, err := smtp.Dial("127.0.0.1:" + testPort)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	err = submitMsg(t, cl, "sender1@example.org", []string{"rcpt1@example.com", "rcpt2@example.com"}, testMsg)
	if err != nil {
		t.Fatal(err)
	}
	err = submitMsg(t, cl, "sender2@example.org", []string{"rcpt3@example.com", "rcpt4@example.com"}, testMsg)
	if err != nil {
		t.Fatal(err)
	}

	if len(tgt.Messages) != 2 {
		t.Fatal("Expected two messages, got", len(tgt.Messages))
	}
	msg := tgt.Messages[0]
	msgID := testutils.CheckMsgID(t, &msg, "sender1@example.org", []string{"rcpt1@example.com", "rcpt2@example.com"}, "")
	receivedPrefix := `from mx.example.org (mx.example.org [127.0.0.1]) by mx.example.com (envelope-sender <sender1@example.org>) with ESMTP id ` + msgID
	if !strings.HasPrefix(msg.Header.Get("Received"), receivedPrefix) {
		t.Error("Wrong Received contents:", msg.Header.Get("Received"))
	}

	msg = tgt.Messages[1]
	msgID = testutils.CheckMsgID(t, &msg, "sender2@example.org", []string{"rcpt3@example.com", "rcpt4@example.com"}, "")
	receivedPrefix = `from mx.example.org (mx.example.org [127.0.0.1]) by mx.example.com (envelope-sender <sender2@example.org>) with ESMTP id ` + msgID
	if !strings.HasPrefix(msg.Header.Get("Received"), receivedPrefix) {
		t.Error("Wrong Received contents:", msg.Header.Get("Received"))
	}
}

func TestSMTPDelivery_AbortData(t *testing.T) {
	tgt := testutils.Target{}
	endp := testEndpoint(t, "smtp", nil, &tgt, nil, nil)
	defer endp.Close()

	cl, err := smtp.Dial("127.0.0.1:" + testPort)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if err := cl.Hello("mx.example.org"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Mail("sender@example.org"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Rcpt("test@example.com"); err != nil {
		t.Fatal(err)
	}
	data, err := cl.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := data.Write([]byte(testMsg)); err != nil {
		t.Fatal(err)
	}

	// Then.. Suddenly, close the connection without sending the final dot.
	cl.Close()

	time.Sleep(250 * time.Millisecond)

	if len(tgt.Messages) != 0 {
		t.Fatal("Expected no messages, got", len(tgt.Messages))
	}
}

func TestSMTPDelivery_AbortLogout(t *testing.T) {
	tgt := testutils.Target{}
	endp := testEndpoint(t, "smtp", nil, &tgt, nil, nil)
	defer endp.Close()

	cl, err := smtp.Dial("127.0.0.1:" + testPort)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if err := cl.Hello("mx.example.org"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Mail("sender@example.org"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Rcpt("test@example.com"); err != nil {
		t.Fatal(err)
	}

	// Then.. Suddenly, close the connection.
	cl.Close()

	time.Sleep(250 * time.Millisecond)

	if len(tgt.Messages) != 0 {
		t.Fatal("Expected no messages, got", len(tgt.Messages))
	}
}

func TestSMTPDelivery_Reset(t *testing.T) {
	tgt := testutils.Target{}
	endp := testEndpoint(t, "smtp", nil, &tgt, nil, nil)
	defer endp.Close()

	cl, err := smtp.Dial("127.0.0.1:" + testPort)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if err := cl.Mail("from-garbage@example.org"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Rcpt("to-garbage@example.org"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Reset(); err != nil {
		t.Fatal(err)
	}

	// then submit the message as if nothing happened.

	err = submitMsg(t, cl, "sender@example.org", []string{"rcpt1@example.com", "rcpt2@example.com"}, testMsg)
	if err != nil {
		t.Fatal(err)
	}

	if len(tgt.Messages) != 1 {
		t.Fatal("Expected a message, got", len(tgt.Messages))
	}
	msg := tgt.Messages[0]
	testutils.CheckMsgID(t, &msg, "sender@example.org", []string{"rcpt1@example.com", "rcpt2@example.com"}, "")
}

func TestSMTPDelivery_SubmissionAuthRequire(t *testing.T) {
	tgt := testutils.Target{}
	endp := testEndpoint(t, "submission", &module.Dummy{}, &tgt, nil, nil)
	defer endp.Close()

	cl, err := smtp.Dial("127.0.0.1:" + testPort)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if err := cl.Mail("from-garbage@example.org"); err == nil {
		t.Fatal("Expected an error, got none")
	}
}

func TestSMTPDelivery_SubmissionAuthOK(t *testing.T) {
	tgt := testutils.Target{}
	endp := testEndpoint(t, "submission", &module.Dummy{}, &tgt, nil, nil)
	defer endp.Close()

	cl, err := smtp.Dial("127.0.0.1:" + testPort)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if err := cl.Auth(sasl.NewPlainClient("", "user", "password")); err != nil {
		t.Fatal(err)
	}

	if err := submitMsg(t, cl, "sender@example.org", []string{"rcpt@example.org"}, testMsg); err != nil {
		t.Fatal(err)
	}

	if len(tgt.Messages) != 1 {
		t.Fatal("Expected a message, got", len(tgt.Messages))
	}
	msg := tgt.Messages[0]
	msgID := testutils.CheckMsgID(t, &msg, "sender@example.org", []string{"rcpt@example.org"}, "")

	if msg.MsgMeta.AuthUser != "user" {
		t.Error("Wrong AuthUser:", msg.MsgMeta.AuthUser)
	}
	if msg.MsgMeta.AuthPassword != "password" {
		t.Error("Wrong AuthPassword:", msg.MsgMeta.AuthPassword)
	}

	receivedPrefix := ` by mx.example.com (envelope-sender <sender@example.org>) with ESMTP id ` + msgID
	if !strings.HasPrefix(msg.Header.Get("Received"), receivedPrefix) {
		t.Error("Wrong Received contents:", msg.Header.Get("Received"))
	}

	if msg.Header.Get("Message-ID") == "" {
		t.Error("No submissionPrepare run")
	}
}

func TestMain(m *testing.M) {
	remoteSmtpPort := flag.String("test.smtpport", "random", "(maddy) SMTP port to use for connections in tests")
	flag.Parse()

	if *remoteSmtpPort == "random" {
		rand.Seed(time.Now().UnixNano())
		*remoteSmtpPort = strconv.Itoa(rand.Intn(65536-10000) + 10000)
	}

	testPort = *remoteSmtpPort
	os.Exit(m.Run())
}
