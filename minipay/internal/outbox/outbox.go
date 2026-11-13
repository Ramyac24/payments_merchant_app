// Package outbox implements the transactional outbox pattern: an event row is
// written in the same DB transaction as the state change it describes, so
// "state changed" and "event will be sent" can never disagree.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/sindhu/minipay/internal/ids"
)

// Event is the envelope delivered to merchants (shape mirrors Stripe events).
type Event struct {
	ID      string    `json:"id"`
	Object  string    `json:"object"`
	Type    string    `json:"type"`
	Created int64     `json:"created"`
	Data    EventData `json:"data"`
}

type EventData struct {
	Object any `json:"object"`
}

// Insert records an event inside tx and returns its id.
func Insert(ctx context.Context, tx *sql.Tx, merchantID, aggregateID, eventType string, object any) (string, error) {
	ev := Event{
		ID:      ids.New("evt"),
		Object:  "event",
		Type:    eventType,
		Created: time.Now().Unix(),
		Data:    EventData{Object: object},
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO outbox (event_id, merchant_id, aggregate_id, event_type, payload)
		VALUES ($1, $2, $3, $4, $5)`, ev.ID, merchantID, aggregateID, eventType, payload)
	return ev.ID, err
}
