package tools

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"julia/internal/core"
)

func TestGmailAppPasswordConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.json")
	raw := `{"username":"me@example.com","password":"app-password","address":"imap.gmail.com:993","security":"tls"}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadMailConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled() || config.Password != "app-password" || config.Address != "imap.gmail.com:993" || config.Security != "tls" {
		t.Fatal("Gmail IMAP configuration changed")
	}
	tlsConfig := config.tlsConfig("imap.gmail.com")
	if tlsConfig.InsecureSkipVerify || tlsConfig.ServerName != "imap.gmail.com" || tlsConfig.MinVersion < tls.VersionTLS12 {
		t.Fatal("Gmail must use verified TLS")
	}
	if strings.Contains(config.Summary(), "app-password") {
		t.Fatal("summary exposed the password")
	}
}

// The memory IMAP server does not advertise Gmail's capability. This small
// server tests the actual LOGIN, LIST and MOVE exchange over implicit TLS.
func gmailFixture(t *testing.T, gmail bool, handle func(string) string) *Executor {
	t.Helper()
	certificate, _ := mailTestCert(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	caps := "IMAP4rev1 MOVE"
	if gmail {
		caps += " X-GM-EXT-1"
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				fmt.Fprintf(conn, "* OK [CAPABILITY %s] fixture\r\n", caps)
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					tag, command, ok := strings.Cut(scanner.Text(), " ")
					if !ok {
						t.Error("invalid IMAP command")
						return
					}
					switch {
					case command == "CAPABILITY":
						fmt.Fprintf(conn, "* CAPABILITY %s\r\n", caps)
					case strings.HasPrefix(command, "LOGIN "):
						if command != `LOGIN "me@example.com" "app-password"` {
							t.Error("LOGIN did not use the configured app password")
						}
						fmt.Fprintf(conn, "%s OK [CAPABILITY %s] authenticated\r\n", tag, caps)
						continue
					case command == "LOGOUT":
						fmt.Fprintf(conn, "* BYE closing\r\n%s OK logout\r\n", tag)
						return
					default:
						io.WriteString(conn, handle(command))
					}
					fmt.Fprintf(conn, "%s OK done\r\n", tag)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		workers.Wait()
	})
	e, _ := fixture(t, "auto", nil)
	e.config.Mail = MailConfig{
		Username: "me@example.com", Password: "app-password",
		Address: listener.Addr().String(), Security: "tls",
	}
	return e
}

func TestGmailArchiveUsesLocalizedAllMailAfterApproval(t *testing.T) {
	var approved atomic.Bool
	var moves atomic.Int32
	e := gmailFixture(t, true, func(command string) string {
		switch {
		case strings.HasPrefix(command, "LIST "):
			return "* LIST (\\Archive) \"/\" \"Archive\"\r\n* LIST (\\All) \"/\" \"[Gmail]/Alle mails\"\r\n"
		case strings.HasPrefix(command, "EXAMINE "):
			return "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n"
		case strings.HasPrefix(command, "SELECT "):
			if !approved.Load() {
				t.Error("writable selection before approval")
			}
			return "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n"
		case strings.HasPrefix(command, "UID FETCH 42 "):
			return "* 1 FETCH (UID 42 FLAGS () ENVELOPE (NIL \"Invoice\" ((\"Jane\" NIL \"jane\" \"example.com\")) NIL NIL NIL NIL NIL NIL NIL))\r\n"
		case command == `UID MOVE 42 "[Gmail]/Alle mails"`:
			if !approved.Load() {
				t.Error("MOVE before approval")
			}
			moves.Add(1)
			return "* 1 EXPUNGE\r\n"
		default:
			t.Errorf("unexpected command: %s", command)
			return ""
		}
	})
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		t.Error("automated reviewer saw archive")
		return core.Decision{}, nil
	}
	approvals := 0
	e.config.Approve = func(_ context.Context, approval core.Approval) (core.Decision, error) {
		approvals++
		var card mailArchiveApproval
		if err := json.Unmarshal(approval.Call.Arguments, &card); err != nil {
			t.Fatal(err)
		}
		if card.Into != "[Gmail]/Alle mails" || card.Subject != "Invoice" || card.From != "Jane <jane@example.com>" {
			t.Fatalf("wrong approval card: %+v", card)
		}
		allow := approvals == 2
		approved.Store(allow)
		return core.Decision{Allow: allow}, nil
	}
	args := mailArgs{ID: "INBOX:1:42"}
	call := core.ToolCall{Name: "Mail"}
	if _, err := e.mailArchive(context.Background(), call, args); err == nil || moves.Load() != 0 {
		t.Fatalf("denial did not prevent MOVE: %v", err)
	}
	if _, err := e.mailArchive(context.Background(), call, args); err != nil || moves.Load() != 1 || approvals != 2 {
		t.Fatalf("approved archive failed: %v, moves=%d, approvals=%d", err, moves.Load(), approvals)
	}
}

func TestGmailArchiveRejectsOtherLabels(t *testing.T) {
	e := gmailFixture(t, true, func(command string) string {
		t.Errorf("unexpected command: %s", command)
		return ""
	})
	e.config.Approve = func(context.Context, core.Approval) (core.Decision, error) {
		t.Error("non-INBOX archive reached approval")
		return core.Decision{Allow: true}, nil
	}
	_, err := e.mailArchive(context.Background(), core.ToolCall{Name: "Mail"}, mailArgs{ID: "[Gmail]/Trash:1:42"})
	if err == nil || !strings.Contains(err.Error(), "only INBOX") {
		t.Fatalf("non-INBOX archive: %v", err)
	}
}

func TestGmailArchiveRequiresAllMail(t *testing.T) {
	e := gmailFixture(t, true, func(command string) string {
		if !strings.HasPrefix(command, "LIST ") {
			t.Errorf("unexpected command: %s", command)
		}
		return "* LIST (\\Archive) \"/\" \"Archive\"\r\n"
	})
	session, err := e.mailConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.archiveMailbox(); err == nil || !strings.Contains(err.Error(), "All Mail is not visible") {
		t.Fatalf("missing Gmail All Mail: %v", err)
	}
}

func TestMailDoesNotUseAllMailForOtherServers(t *testing.T) {
	e := gmailFixture(t, false, func(command string) string {
		if !strings.HasPrefix(command, "LIST ") {
			t.Errorf("unexpected command: %s", command)
		}
		return "* LIST (\\All) \"/\" \"All Mail\"\r\n"
	})
	session, err := e.mailConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.archiveMailbox(); err == nil || !strings.Contains(err.Error(), "no Archive mailbox") {
		t.Fatalf("generic server used All Mail: %v", err)
	}
}
