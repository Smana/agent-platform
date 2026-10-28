// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The Slack self-serve link's bounds (docs/superpowers/specs/2026-10-28-slack-self-serve-link-design.md).
const (
	// MaxSlackCodes bounds the live codes in all: each Slack user holds one at most.
	MaxSlackCodes = 1000
	// MaxSlackLinks bounds the Slack users one ZITADEL account holds.
	MaxSlackLinks = 5
)

// The link's refusals, each a reason the page names.
var (
	ErrCodeInvalid  = errors.New("the link code is unknown, expired or used")
	ErrTooManyCodes = errors.New("too many link codes are waiting")
	ErrTooManyLinks = errors.New("the account holds the most Slack users it may")
	// ErrAlreadyLinked is a Slack user linked to another account: a link is never moved silently,
	// it is unlinked first.
	ErrAlreadyLinked = errors.New("the Slack user is linked to another account")
)

// SlackCode is what a code binds: the Slack user it was minted for, as the App saw them (their
// handle and display name, display only), and when it expires.
type SlackCode struct {
	Team, User, Handle, Name string
	Expires                  time.Time
}

// SlackLink is one Slack user linked to an account.
type SlackLink struct {
	Team, User, Handle, Name string
	LinkedAt                 time.Time
}

// MintSlackCode stores a code by its hash for c's Slack user, replacing that user's earlier code,
// after purging every expired or used one. It refuses once MaxSlackCodes are live.
func (s *Store) MintSlackCode(ctx context.Context, codeSHA256 []byte, c SlackCode, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One mint at a time, so the bound holds against concurrent mints.
	if _, err := tx.Exec(ctx, `LOCK TABLE slack_link_codes IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM slack_link_codes WHERE expires_at <= $1 OR used_at IS NOT NULL
		OR (team_id = $2 AND user_id = $3)`, now, c.Team, c.User); err != nil {
		return err
	}
	var live int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM slack_link_codes`).Scan(&live); err != nil {
		return err
	}
	if live >= MaxSlackCodes {
		return ErrTooManyCodes
	}
	if _, err := tx.Exec(ctx, `INSERT INTO slack_link_codes (code_sha256, team_id, user_id, handle, display_name, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, codeSHA256, c.Team, c.User, c.Handle, c.Name, c.Expires); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SlackCode reads a live code without using it: ErrCodeInvalid when it is unknown, expired or used.
func (s *Store) SlackCode(ctx context.Context, codeSHA256 []byte, now time.Time) (SlackCode, error) {
	var c SlackCode
	err := s.pool.QueryRow(ctx, `SELECT team_id, user_id, handle, display_name, expires_at FROM slack_link_codes
		WHERE code_sha256 = $1 AND used_at IS NULL AND expires_at > $2`, codeSHA256, now).Scan(&c.Team, &c.User, &c.Handle, &c.Name, &c.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrCodeInvalid
	}
	return c, err
}

// LinkSlack uses a live code of team once and links its Slack user to sub. A Slack user linked to
// another account is ErrAlreadyLinked, and an account past MaxSlackLinks ErrTooManyLinks; either
// leaves the code unused. A link to the same account is refreshed.
func (s *Store) LinkSlack(ctx context.Context, codeSHA256 []byte, team, sub string, now time.Time) (SlackCode, error) {
	var c SlackCode
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return c, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = tx.QueryRow(ctx, `UPDATE slack_link_codes SET used_at = $3
		WHERE code_sha256 = $1 AND team_id = $2 AND used_at IS NULL AND expires_at > $3
		RETURNING team_id, user_id, handle, display_name, expires_at`, codeSHA256, team, now).Scan(&c.Team, &c.User, &c.Handle, &c.Name, &c.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrCodeInvalid
	}
	if err != nil {
		return c, err
	}
	// The account's row lock: two confirms for one account count each other.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('slack_links:' || $1, 0))`, sub); err != nil {
		return c, err
	}
	var holder string
	err = tx.QueryRow(ctx, `SELECT sub FROM slack_links WHERE team_id = $1 AND user_id = $2 FOR UPDATE`, c.Team, c.User).Scan(&holder)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return c, err
	case holder != sub:
		return c, ErrAlreadyLinked
	}
	var others int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM slack_links WHERE sub = $1 AND NOT (team_id = $2 AND user_id = $3)`,
		sub, c.Team, c.User).Scan(&others); err != nil {
		return c, err
	}
	if others >= MaxSlackLinks {
		return c, ErrTooManyLinks
	}
	// The conflict is the same account's only (checked above, under the row lock); a racing first
	// link to another account loses on the primary key and the confirm is refused.
	tag, err := tx.Exec(ctx, `INSERT INTO slack_links (team_id, user_id, handle, display_name, sub, linked_at) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (team_id, user_id) DO UPDATE SET handle = EXCLUDED.handle, display_name = EXCLUDED.display_name, linked_at = EXCLUDED.linked_at
		WHERE slack_links.sub = EXCLUDED.sub`,
		c.Team, c.User, c.Handle, c.Name, sub, now)
	if err != nil {
		return c, err
	}
	if tag.RowsAffected() == 0 {
		return c, ErrAlreadyLinked
	}
	return c, tx.Commit(ctx)
}

// SlackLink is the ZITADEL sub a Slack user is linked to; found is false when none is.
func (s *Store) SlackLink(ctx context.Context, team, user string) (sub string, found bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT sub FROM slack_links WHERE team_id = $1 AND user_id = $2`, team, user).Scan(&sub)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return sub, err == nil, err
}

// SlackLinksOf lists the account's Slack users, oldest link first.
func (s *Store) SlackLinksOf(ctx context.Context, sub string) ([]SlackLink, error) {
	rows, err := s.pool.Query(ctx, `SELECT team_id, user_id, handle, display_name, linked_at FROM slack_links
		WHERE sub = $1 ORDER BY linked_at, team_id, user_id`, sub)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SlackLink, error) {
		var l SlackLink
		return l, r.Scan(&l.Team, &l.User, &l.Handle, &l.Name, &l.LinkedAt)
	})
}

// UnlinkSlack removes a Slack user's link: only when it is sub's, or whoever's it is when sub is
// "" (the Slack user's own request, through the App). gone is false when there was nothing to remove.
func (s *Store) UnlinkSlack(ctx context.Context, team, user, sub string) (gone bool, err error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM slack_links WHERE team_id = $1 AND user_id = $2 AND ($3 = '' OR sub = $3)`, team, user, sub)
	return tag.RowsAffected() > 0, err
}
