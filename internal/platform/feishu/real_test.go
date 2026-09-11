package feishu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractCallbackToken(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantToken     string
		wantEncrypted bool
	}{
		{
			name:      "schema 1.0 token",
			body:      `{"token":"tok_v1","type":"event_callback"}`,
			wantToken: "tok_v1",
		},
		{
			name:      "schema 2.0 header token",
			body:      `{"schema":"2.0","header":{"token":"tok_v2","event_type":"x"}}`,
			wantToken: "tok_v2",
		},
		{
			name:      "header wins over top level",
			body:      `{"token":"top","header":{"token":"hdr"}}`,
			wantToken: "hdr",
		},
		{
			name:      "empty header token falls back to top level",
			body:      `{"token":"top","header":{"event_type":"x"}}`,
			wantToken: "top",
		},
		{
			name:          "encrypted payload hides the token",
			body:          `{"encrypt":"BASE64BLOB"}`,
			wantToken:     "",
			wantEncrypted: true,
		},
		{
			name:          "encrypt wins even with a top level token",
			body:          `{"encrypt":"blob","token":"ignored"}`,
			wantToken:     "",
			wantEncrypted: true,
		},
		{
			name:      "no token at all",
			body:      `{"schema":"2.0"}`,
			wantToken: "",
		},
		{
			name:      "invalid json",
			body:      `not json`,
			wantToken: "",
		},
		{
			name:      "empty body",
			body:      ``,
			wantToken: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			token, encrypted := extractCallbackToken([]byte(tc.body))
			if token != tc.wantToken {
				t.Errorf("token = %q, want %q", token, tc.wantToken)
			}
			if encrypted != tc.wantEncrypted {
				t.Errorf("encrypted = %v, want %v", encrypted, tc.wantEncrypted)
			}
		})
	}
}

func TestRealDownloader_Validation(t *testing.T) {
	t.Run("requires app id", func(t *testing.T) {
		if _, err := RealDownloader("", "secret", "", t.TempDir()); err == nil {
			t.Error("want error for missing app_id")
		}
	})
	t.Run("requires a directory", func(t *testing.T) {
		if _, err := RealDownloader("cli_x", "secret", "", ""); err == nil {
			t.Error("want error for missing dir")
		}
	})
}

func TestRealDownloader_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "attachments")
	d, err := RealDownloader("cli_x", "secret", "", dir)
	if err != nil {
		t.Fatalf("RealDownloader: %v", err)
	}
	if d == nil {
		t.Fatal("RealDownloader returned a nil downloader")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("download dir was not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("download path is not a directory")
	}
}

func TestRealDownloader_LarkDomainAccepted(t *testing.T) {
	// A non-default domain must be accepted (the Lark international endpoint).
	d, err := RealDownloader("cli_x", "secret", "https://open.larksuite.com", t.TempDir())
	if err != nil {
		t.Fatalf("RealDownloader with a Lark domain: %v", err)
	}
	if d == nil {
		t.Fatal("RealDownloader returned a nil downloader")
	}
}

func TestRealSender_Validation(t *testing.T) {
	if _, err := RealSender("", "secret", ""); err == nil {
		t.Error("want error for missing app_id")
	}
	if _, _, _, err := RealSenderFuncs("", "secret", ""); err == nil {
		t.Error("want error from RealSenderFuncs for missing app_id")
	}
}

func TestRealSender_ConstructsAllPrimitives(t *testing.T) {
	s, err := RealSender("cli_x", "secret", "")
	if err != nil {
		t.Fatalf("RealSender: %v", err)
	}
	sender, ok := s.(*larkSender)
	if !ok {
		t.Fatalf("RealSender returned %T, want *larkSender", s)
	}
	if sender.create == nil {
		t.Error("create primitive is nil")
	}
	if sender.patch == nil {
		t.Error("patch primitive is nil (UpdateCard would be unavailable)")
	}
	if sender.recall == nil {
		t.Error("recall primitive is nil")
	}
}

func TestRealSenderFuncs_AllNonNil(t *testing.T) {
	create, patch, recall, err := RealSenderFuncs("cli_x", "secret", "https://open.larksuite.com")
	if err != nil {
		t.Fatalf("RealSenderFuncs: %v", err)
	}
	if create == nil || patch == nil || recall == nil {
		t.Error("RealSenderFuncs returned a nil primitive")
	}
}
