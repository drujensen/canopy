// Package interfaces holds repository/source contracts the domain layer
// depends on. Concrete implementations live under internal/impl and are
// never imported back into domain/entities or domain/interfaces (Design §2).
package interfaces

import (
	"context"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// ChatRepository persists Chat entities (Design §6, Requirements FR13). JSON
// is the only implementation in v1 (internal/impl/repositories/json), but
// domain code depends only on this interface.
type ChatRepository interface {
	// Create persists a new chat. It returns an error if a chat with the
	// same ID already exists.
	Create(ctx context.Context, chat *entities.Chat) error

	// Get returns the chat with the given ID, or an error if it doesn't
	// exist.
	Get(ctx context.Context, id string) (*entities.Chat, error)

	// Update overwrites an existing chat's stored state. It returns an
	// error if the chat doesn't already exist.
	Update(ctx context.Context, chat *entities.Chat) error

	// Delete removes the chat with the given ID. It returns an error if
	// the chat doesn't exist.
	Delete(ctx context.Context, id string) error

	// List returns every persisted chat.
	List(ctx context.Context) ([]*entities.Chat, error)
}
