// Package json implements domain/interfaces repositories backed by JSON
// files on disk (Design §2/§6) — the only storage backend in v1.
package json

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/domain/interfaces"
)

// ErrChatNotFound is returned by Get/Update/Delete when no chat with the
// given ID exists.
var ErrChatNotFound = errors.New("chat not found")

// ErrChatAlreadyExists is returned by Create when a chat with the given ID
// already exists.
var ErrChatAlreadyExists = errors.New("chat already exists")

// ChatRepository is a JSON-file-backed implementation of
// interfaces.ChatRepository (Design §2/§6). Each chat is stored as its own
// file, "<root>/<id>.json". Writes are atomic: the new content is written to
// a temporary file in the same directory and then moved into place with
// os.Rename, which is atomic on the same filesystem. That means concurrent
// writes to different chats never interleave, and a crash mid-write never
// leaves a torn/partial chat file — a reader either sees the old content or
// the fully-written new content, never a mix.
type ChatRepository struct {
	root string
}

var _ interfaces.ChatRepository = (*ChatRepository)(nil)

// NewChatRepository returns a ChatRepository storing one JSON file per chat
// under root, creating root (and any missing parents) if needed.
func NewChatRepository(root string) (*ChatRepository, error) {
	if root == "" {
		return nil, fmt.Errorf("chat repository: storage root must not be empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("chat repository: failed to create storage root %q: %w", root, err)
	}
	return &ChatRepository{root: root}, nil
}

func (r *ChatRepository) path(id string) string {
	return filepath.Join(r.root, id+".json")
}

// Create persists a new chat. It fails with ErrChatAlreadyExists if a chat
// with the same ID is already stored.
func (r *ChatRepository) Create(ctx context.Context, chat *entities.Chat) error {
	if chat.ID == "" {
		return fmt.Errorf("chat repository: chat ID must not be empty")
	}
	if _, err := os.Stat(r.path(chat.ID)); err == nil {
		return fmt.Errorf("chat repository: create %q: %w", chat.ID, ErrChatAlreadyExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("chat repository: failed to stat chat %q: %w", chat.ID, err)
	}
	return r.write(chat)
}

// Get returns the chat with the given ID, or ErrChatNotFound if it doesn't
// exist.
func (r *ChatRepository) Get(ctx context.Context, id string) (*entities.Chat, error) {
	data, err := os.ReadFile(r.path(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("chat repository: get %q: %w", id, ErrChatNotFound)
		}
		return nil, fmt.Errorf("chat repository: failed to read chat %q: %w", id, err)
	}
	var chat entities.Chat
	if err := json.Unmarshal(data, &chat); err != nil {
		return nil, fmt.Errorf("chat repository: failed to unmarshal chat %q: %w", id, err)
	}
	return &chat, nil
}

// Update overwrites an existing chat's stored state. It fails with
// ErrChatNotFound if the chat doesn't already exist.
func (r *ChatRepository) Update(ctx context.Context, chat *entities.Chat) error {
	if chat.ID == "" {
		return fmt.Errorf("chat repository: chat ID must not be empty")
	}
	if _, err := os.Stat(r.path(chat.ID)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("chat repository: update %q: %w", chat.ID, ErrChatNotFound)
		}
		return fmt.Errorf("chat repository: failed to stat chat %q: %w", chat.ID, err)
	}
	return r.write(chat)
}

// Delete removes the chat with the given ID, or fails with ErrChatNotFound
// if it doesn't exist.
func (r *ChatRepository) Delete(ctx context.Context, id string) error {
	if err := os.Remove(r.path(id)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("chat repository: delete %q: %w", id, ErrChatNotFound)
		}
		return fmt.Errorf("chat repository: failed to delete chat %q: %w", id, err)
	}
	return nil
}

// List returns every persisted chat.
func (r *ChatRepository) List(ctx context.Context) ([]*entities.Chat, error) {
	dirEntries, err := os.ReadDir(r.root)
	if err != nil {
		return nil, fmt.Errorf("chat repository: failed to list storage root %q: %w", r.root, err)
	}
	chats := make([]*entities.Chat, 0, len(dirEntries))
	for _, e := range dirEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		chat, err := r.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		chats = append(chats, chat)
	}
	return chats, nil
}

// write serializes chat to JSON and writes it atomically: a temp file is
// created in the same directory (guaranteeing the final rename stays on one
// filesystem), fully written and synced, then renamed over the destination
// path. No package-level lock is needed for correctness across different
// chat IDs — os.CreateTemp already guarantees a unique temp file per call,
// and os.Rename is atomic — but two concurrent writers to the *same* chat ID
// will race on which write's rename lands last (last writer wins; neither
// can produce a torn file).
func (r *ChatRepository) write(chat *entities.Chat) error {
	data, err := json.MarshalIndent(chat, "", "  ")
	if err != nil {
		return fmt.Errorf("chat repository: failed to marshal chat %q: %w", chat.ID, err)
	}

	dest := r.path(chat.ID)
	tmp, err := os.CreateTemp(r.root, ".tmp-"+chat.ID+"-*")
	if err != nil {
		return fmt.Errorf("chat repository: failed to create temp file for chat %q: %w", chat.ID, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: once the rename below succeeds this is a no-op
	// (the file no longer exists at tmpName), so the error is ignored.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("chat repository: failed to write chat %q: %w", chat.ID, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("chat repository: failed to sync chat %q: %w", chat.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("chat repository: failed to close temp file for chat %q: %w", chat.ID, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("chat repository: failed to rename temp file into place for chat %q: %w", chat.ID, err)
	}
	return nil
}
