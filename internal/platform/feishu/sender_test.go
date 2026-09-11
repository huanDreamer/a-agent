package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// okCreateResp builds a successful create response carrying a message id.
func okCreateResp(id string) *im.CreateMessageResp {
	return &im.CreateMessageResp{
		CodeError: larkcore.CodeError{Code: 0, Msg: "ok"},
		Data:      &im.CreateMessageRespData{MessageId: strp(id)},
	}
}

// fakePatcher is a stub lark patch API.
type fakePatcher struct {
	calls int
	err   error
	resp  *im.PatchMessageResp
}

func (f *fakePatcher) Patch(_ context.Context, _ *im.PatchMessageReq, _ ...larkcore.RequestOptionFunc) (*im.PatchMessageResp, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &im.PatchMessageResp{CodeError: larkcore.CodeError{Code: 0}}, nil
}

// fakeRecaller is a stub lark delete API.
type fakeRecaller struct {
	calls int
	err   error
	resp  *im.DeleteMessageResp
}

func (f *fakeRecaller) Delete(_ context.Context, _ *im.DeleteMessageReq, _ ...larkcore.RequestOptionFunc) (*im.DeleteMessageResp, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &im.DeleteMessageResp{CodeError: larkcore.CodeError{Code: 0}}, nil
}

func TestSender_SendText(t *testing.T) {
	fs := &fakeSend{}
	s := NewLarkSenderWithCreate(func(ctx context.Context, rid, ridt, mt, content string) (string, error) {
		if err := fs.send(ctx, rid, ridt, mt, content); err != nil {
			return "", err
		}
		return "om_42", nil
	})

	id, err := s.SendText(context.Background(), "oc_1", "hello")
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if id != "om_42" {
		t.Errorf("id = %q, want om_42", id)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("sends = %d, want 1", len(fs.calls))
	}
	c := fs.calls[0]
	if c.receiveID != "oc_1" || c.receiveIDType != "chat_id" || c.msgType != "text" {
		t.Errorf("unexpected call: %+v", c)
	}
	if !strings.Contains(c.content, `"text":"hello"`) {
		t.Errorf("content = %s, want hello payload", c.content)
	}
}

func TestSender_SendCard(t *testing.T) {
	fs := &fakeSend{}
	s := NewLarkSenderWithCreate(func(ctx context.Context, rid, ridt, mt, content string) (string, error) {
		if err := fs.send(ctx, rid, ridt, mt, content); err != nil {
			return "", err
		}
		return "om_card", nil
	})

	id, err := s.SendCard(context.Background(), "oc_1", "My Title", "**bold**")
	if err != nil {
		t.Fatalf("SendCard: %v", err)
	}
	if id != "om_card" {
		t.Errorf("id = %q, want om_card", id)
	}
	c := fs.calls[0]
	if c.msgType != "interactive" {
		t.Errorf("msgType = %q, want interactive", c.msgType)
	}

	var card struct {
		Header struct {
			Title struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"title"`
		} `json:"header"`
		Elements []CardElement `json:"elements"`
	}
	if err := json.Unmarshal([]byte(c.content), &card); err != nil {
		t.Fatalf("card JSON invalid: %v (%s)", err, c.content)
	}
	if card.Header.Title.Content != "My Title" {
		t.Errorf("title = %q, want My Title", card.Header.Title.Content)
	}
	if card.Header.Title.Tag != "plain_text" {
		t.Errorf("title tag = %q, want plain_text", card.Header.Title.Tag)
	}
	if len(card.Elements) == 0 {
		t.Error("card has no elements")
	}
}

func TestSender_SendCardDerivesTitle(t *testing.T) {
	fs := &fakeSend{}
	s := NewLarkSenderWithCreate(func(ctx context.Context, rid, ridt, mt, content string) (string, error) {
		return "x", fs.send(ctx, rid, ridt, mt, content)
	})

	if _, err := s.SendCard(context.Background(), "oc_1", "", "# 我的标题\n\n正文"); err != nil {
		t.Fatalf("SendCard: %v", err)
	}
	if !strings.Contains(fs.calls[0].content, "我的标题") {
		t.Errorf("card content did not include the derived title: %s", fs.calls[0].content)
	}
}

func TestSender_UpdateCard(t *testing.T) {
	fs := &fakeSend{}
	p := &fakePatcher{}
	s := NewLarkSenderWithCreate(
		func(ctx context.Context, rid, ridt, mt, content string) (string, error) {
			return "om_1", fs.send(ctx, rid, ridt, mt, content)
		},
		WithPatcher(patchViaLark(p)),
	)

	if err := s.UpdateCard(context.Background(), "om_1", "T", "new body"); err != nil {
		t.Fatalf("UpdateCard: %v", err)
	}
	if p.calls != 1 {
		t.Errorf("patch calls = %d, want 1", p.calls)
	}

	t.Run("requires message id", func(t *testing.T) {
		if err := s.UpdateCard(context.Background(), "", "T", "x"); err == nil {
			t.Error("want error for empty message id")
		}
	})
	t.Run("unsupported without a patcher", func(t *testing.T) {
		plain := NewLarkSenderWithCreate(func(context.Context, string, string, string, string) (string, error) {
			return "x", nil
		})
		err := plain.UpdateCard(context.Background(), "om_1", "T", "x")
		if err == nil || !strings.Contains(err.Error(), "does not support") {
			t.Errorf("err = %v, want an unsupported error", err)
		}
	})
}

func TestSender_Recall(t *testing.T) {
	r := &fakeRecaller{}
	s := NewLarkSenderWithCreate(
		func(context.Context, string, string, string, string) (string, error) { return "x", nil },
		WithRecaller(recallViaLark(r)),
	)

	if err := s.Recall(context.Background(), "om_1"); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if r.calls != 1 {
		t.Errorf("recall calls = %d, want 1", r.calls)
	}
	if err := s.Recall(context.Background(), ""); err == nil {
		t.Error("want error for empty message id")
	}

	plain := NewLarkSenderWithCreate(func(context.Context, string, string, string, string) (string, error) {
		return "x", nil
	})
	if err := plain.Recall(context.Background(), "om_1"); err == nil {
		t.Error("want error when the sender has no recall support")
	}
}

func TestSender_NilCreateRejected(t *testing.T) {
	s := NewLarkSenderWithCreate(nil)
	if _, err := s.SendText(context.Background(), "oc", "x"); err == nil {
		t.Error("SendText should fail without a create primitive")
	}
	if _, err := s.SendCard(context.Background(), "oc", "t", "m"); err == nil {
		t.Error("SendCard should fail without a create primitive")
	}
	if err := s.ReplyText(context.Background(), Inbound{ChatID: "oc"}, "x"); err == nil {
		t.Error("ReplyText should fail without a create primitive")
	}
}

func TestSender_PropagatesCreateError(t *testing.T) {
	boom := errors.New("boom")
	s := NewLarkSenderWithCreate(func(context.Context, string, string, string, string) (string, error) {
		return "", boom
	})
	if _, err := s.SendText(context.Background(), "oc", "x"); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
}

func TestCreateViaLark(t *testing.T) {
	t.Run("success returns message id", func(t *testing.T) {
		fc := &fakeCreator{resp: okCreateResp("om_7")}
		id, err := createViaLark(fc)(context.Background(), "oc", "chat_id", "text", `{"text":"hi"}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "om_7" {
			t.Errorf("id = %q, want om_7", id)
		}
		if fc.calls != 1 {
			t.Errorf("calls = %d, want 1", fc.calls)
		}
	})
	t.Run("api error code is reported", func(t *testing.T) {
		fc := &fakeCreator{resp: &im.CreateMessageResp{CodeError: larkcore.CodeError{Code: 99991663, Msg: "no permission"}}}
		_, err := createViaLark(fc)(context.Background(), "oc", "chat_id", "text", "{}")
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), "99991663") {
			t.Errorf("err = %v, want it to carry the code", err)
		}
		if !NonRetryable(err) {
			t.Error("a 99991663 permission error should be classified non-retryable")
		}
	})
	t.Run("missing data yields empty id", func(t *testing.T) {
		fc := &fakeCreator{resp: &im.CreateMessageResp{CodeError: larkcore.CodeError{Code: 0}}}
		id, err := createViaLark(fc)(context.Background(), "oc", "chat_id", "text", "{}")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "" {
			t.Errorf("id = %q, want empty", id)
		}
	})
	t.Run("transport error propagates", func(t *testing.T) {
		boom := errors.New("net down")
		fc := &fakeCreator{err: boom}
		if _, err := createViaLark(fc)(context.Background(), "oc", "chat_id", "text", "{}"); !errors.Is(err, boom) {
			t.Errorf("err = %v, want boom", err)
		}
	})
}

func TestPatchViaLark(t *testing.T) {
	p := &fakePatcher{}
	if err := patchViaLark(p)(context.Background(), "om_1", `{"a":1}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.calls != 1 {
		t.Errorf("calls = %d, want 1", p.calls)
	}

	pErr := &fakePatcher{resp: &im.PatchMessageResp{CodeError: larkcore.CodeError{Code: 230002, Msg: "bad"}}}
	err := patchViaLark(pErr)(context.Background(), "om_1", "{}")
	if err == nil || !strings.Contains(err.Error(), "230002") {
		t.Errorf("err = %v, want it to carry the code", err)
	}

	boom := errors.New("boom")
	if err := patchViaLark(&fakePatcher{err: boom})(context.Background(), "om_1", "{}"); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
}

func TestRecallViaLark(t *testing.T) {
	r := &fakeRecaller{}
	if err := recallViaLark(r)(context.Background(), "om_1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.calls != 1 {
		t.Errorf("calls = %d, want 1", r.calls)
	}

	rErr := &fakeRecaller{resp: &im.DeleteMessageResp{CodeError: larkcore.CodeError{Code: 230002, Msg: "bad"}}}
	if err := recallViaLark(rErr)(context.Background(), "om_1"); err == nil {
		t.Error("want error for a non-success response")
	}

	boom := errors.New("boom")
	if err := recallViaLark(&fakeRecaller{err: boom})(context.Background(), "om_1"); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
}

func TestBuildCard(t *testing.T) {
	out, err := buildCard("T", "one\n\n```go\nfmt.Println()\n```")
	if err != nil {
		t.Fatalf("buildCard: %v", err)
	}
	if !strings.Contains(out, "T") {
		t.Error("card is missing the title")
	}
	if !strings.Contains(out, "fmt.Println()") {
		t.Error("card is missing the code block content")
	}
	var card cardBody
	if err := json.Unmarshal([]byte(out), &card); err != nil {
		t.Fatalf("invalid card JSON: %v", err)
	}
	if card.Elements == nil {
		t.Error("card elements should not be nil")
	}
}

func TestBuildText(t *testing.T) {
	out, err := buildText(`quote " and \ backslash`)
	if err != nil {
		t.Fatalf("buildText: %v", err)
	}
	var payload textPayload
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid text JSON: %v", err)
	}
	if payload.Text != `quote " and \ backslash` {
		t.Errorf("text = %q, round trip failed", payload.Text)
	}
}

func TestSenderOptions(t *testing.T) {
	p := &fakePatcher{}
	r := &fakeRecaller{}
	s := NewLarkSenderWithCreate(
		func(context.Context, string, string, string, string) (string, error) { return "x", nil },
		WithPatcher(patchViaLark(p)), WithRecaller(recallViaLark(r)),
	).(*larkSender)
	if s.patch == nil || s.recall == nil {
		t.Error("options were not applied")
	}
}
