package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMediaAsset_RoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	asset := MediaAsset{
		ID: "asset-1", SessionID: "sess-1", Kind: MediaKindImage,
		Path: "media/2026/09/asset-1.png", MIME: "image/png",
		Bytes: 1234, SHA256: strings.Repeat("ab", 32),
	}
	if err := st.CreateMediaAsset(ctx, asset); err != nil {
		t.Fatalf("CreateMediaAsset: %v", err)
	}

	got, err := st.GetMediaAsset(ctx, "asset-1")
	if err != nil {
		t.Fatalf("GetMediaAsset: %v", err)
	}
	if got.SessionID != asset.SessionID || got.Kind != asset.Kind || got.Path != asset.Path ||
		got.MIME != asset.MIME || got.Bytes != asset.Bytes || got.SHA256 != asset.SHA256 {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt was not set")
	}

	if _, err := st.GetMediaAsset(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown asset error = %v, want ErrNotFound", err)
	}
}

func TestMediaAsset_RequiresIDAndPath(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.CreateMediaAsset(ctx, MediaAsset{Path: "media/a.png"}); err == nil {
		t.Error("an asset without an id must be refused")
	}
	if err := st.CreateMediaAsset(ctx, MediaAsset{ID: "x"}); err == nil {
		t.Error("an asset without a path must be refused")
	}
}

func TestMediaAsset_ListIsSessionScoped(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	for _, a := range []MediaAsset{
		{ID: "a1", SessionID: "s1", Kind: MediaKindImage, Path: "media/1.png", MIME: "image/png"},
		{ID: "a2", SessionID: "s1", Kind: MediaKindAudio, Path: "media/2.mp3", MIME: "audio/mpeg"},
		{ID: "b1", SessionID: "s2", Kind: MediaKindImage, Path: "media/3.png", MIME: "image/png"},
	} {
		if err := st.CreateMediaAsset(ctx, a); err != nil {
			t.Fatalf("CreateMediaAsset(%s): %v", a.ID, err)
		}
	}

	one, err := st.ListMediaAssets(ctx, "s1")
	if err != nil {
		t.Fatalf("ListMediaAssets: %v", err)
	}
	if len(one) != 2 {
		t.Fatalf("session s1 has %d assets, want 2", len(one))
	}
	// A session's attachments must never include another session's, which is
	// what keeps an id from being replayed across conversations.
	for _, a := range one {
		if a.SessionID != "s1" {
			t.Errorf("listing s1 returned %s from %s", a.ID, a.SessionID)
		}
	}

	all, err := st.ListMediaAssets(ctx, "")
	if err != nil {
		t.Fatalf("ListMediaAssets(all): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unfiltered listing has %d assets, want 3", len(all))
	}
}

func TestMediaAsset_FindBySHA256(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	digest := strings.Repeat("cd", 32)
	if err := st.CreateMediaAsset(ctx, MediaAsset{
		ID: "a1", SessionID: "s1", Kind: MediaKindImage,
		Path: "media/1.png", MIME: "image/png", Bytes: 10, SHA256: digest,
	}); err != nil {
		t.Fatalf("CreateMediaAsset: %v", err)
	}

	// The digest is how an identical re-upload is recognised instead of being
	// stored a second time.
	got, err := st.FindMediaAssetBySHA256(ctx, "s1", digest)
	if err != nil {
		t.Fatalf("FindMediaAssetBySHA256: %v", err)
	}
	if got.ID != "a1" {
		t.Errorf("found %s, want a1", got.ID)
	}

	if _, err := st.FindMediaAssetBySHA256(ctx, "s2", digest); !errors.Is(err, ErrNotFound) {
		t.Errorf("another session's asset error = %v, want ErrNotFound", err)
	}
	if _, err := st.FindMediaAssetBySHA256(ctx, "s1", strings.Repeat("ef", 32)); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown digest error = %v, want ErrNotFound", err)
	}
	if _, err := st.FindMediaAssetBySHA256(ctx, "s1", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty digest error = %v, want ErrNotFound", err)
	}
}

func TestChatMessage_AttachmentsRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.CreateChatSession(ctx, ChatSession{ID: "s1"}); err != nil {
		t.Fatalf("CreateChatSession: %v", err)
	}
	const ids = `["a1","a2"]`
	if _, err := st.AppendChatMessage(ctx, "s1", ChatMessage{
		Role: RoleUser, Content: "看这张图", Attachments: ids,
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	// A message without attachments must keep an empty value, not a stale one.
	if _, err := st.AppendChatMessage(ctx, "s1", ChatMessage{Role: RoleAssistant, Content: "好的"}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}

	msgs, err := st.ListChatMessages(ctx, "s1", 0)
	if err != nil {
		t.Fatalf("ListChatMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].Attachments != ids {
		t.Errorf("attachments = %q, want %q", msgs[0].Attachments, ids)
	}
	if msgs[1].Attachments != "" {
		t.Errorf("assistant attachments = %q, want empty", msgs[1].Attachments)
	}
}
