package auth

import (
	"context"
	"database/sql"
	"time"
)

// ActivityEvent represents a user activity
type ActivityEvent struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Action    string    `json:"action"`
	Resource  string    `json:"resource"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	Metadata  string    `json:"metadata,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// ActivityTracker tracks user activities
type ActivityTracker struct {
	db *sql.DB
}

// NewActivityTracker creates a new activity tracker
func NewActivityTracker(db *sql.DB) *ActivityTracker {
	return &ActivityTracker{db: db}
}

// InitializeSchema creates activity tables
func (at *ActivityTracker) InitializeSchema(ctx context.Context) error {
	query := `
	CREATE TABLE IF NOT EXISTS user_activities (
		id VARCHAR(36) PRIMARY KEY,
		user_id VARCHAR(255) NOT NULL,
		action VARCHAR(50) NOT NULL,
		resource VARCHAR(255),
		ip VARCHAR(45),
		user_agent TEXT,
		metadata TEXT,
		timestamp TIMESTAMP DEFAULT NOW()
	)`

	_, err := at.db.ExecContext(ctx, query)
	if err != nil {
		return err
	}

	// Create indexes
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_user_activities_user_id ON user_activities(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_user_activities_timestamp ON user_activities(timestamp)`,
	}

	for _, idx := range indexes {
		if _, err := at.db.ExecContext(ctx, idx); err != nil {
			return err
		}
	}

	return nil
}

// Track records an activity event
func (at *ActivityTracker) Track(ctx context.Context, event *ActivityEvent) error {
	if event.ID == "" {
		event.ID = generateID()
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	query := `
	INSERT INTO user_activities (id, user_id, action, resource, ip, user_agent, metadata, timestamp)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	_, err := at.db.ExecContext(ctx, query,
		event.ID, event.UserID, event.Action, event.Resource,
		event.IP, event.UserAgent, event.Metadata, event.Timestamp)

	return err
}

// GetUserActivities retrieves activities for a user
func (at *ActivityTracker) GetUserActivities(ctx context.Context, userID string, limit int) ([]*ActivityEvent, error) {
	query := `
	SELECT id, user_id, action, resource, ip, user_agent, metadata, timestamp
	FROM user_activities
	WHERE user_id = $1
	ORDER BY timestamp DESC
	LIMIT $2`

	rows, err := at.db.QueryContext(ctx, query, userID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var activities []*ActivityEvent
	for rows.Next() {
		var a ActivityEvent
		var metadata sql.NullString
		err := rows.Scan(&a.ID, &a.UserID, &a.Action, &a.Resource,
			&a.IP, &a.UserAgent, &metadata, &a.Timestamp)
		if err != nil {
			return nil, err
		}
		if metadata.Valid {
			a.Metadata = metadata.String
		}
		activities = append(activities, &a)
	}

	return activities, rows.Err()
}
