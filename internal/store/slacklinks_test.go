// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"
)

func codeHash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// A code is read until it expires or is used; consuming it links the Slack user to the sub once
// (replay is code_invalid); a new code for the same user replaces the earlier one.
func TestSlackLinkCodeIsSingleUseAndShortLived(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	now := time.Now().UTC().Truncate(time.Second)
	c := SlackCode{Team: "T01ABC", User: "U02DEF", Name: "dana", Expires: now.Add(10 * time.Minute)}
	if err := s.MintSlackCode(ctx, codeHash("one"), c, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.SlackCode(ctx, codeHash("one"), now)
	if err != nil || got.Team != "T01ABC" || got.User != "U02DEF" || got.Name != "dana" {
		t.Fatalf("peek: %+v %v", got, err)
	}
	if _, err := s.SlackCode(ctx, codeHash("one"), now.Add(11*time.Minute)); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("an expired code still reads: %v", err)
	}
	if _, err := s.LinkSlack(ctx, codeHash("one"), "T01ABC", "zsub1", now.Add(11*time.Minute)); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("an expired code links: %v", err)
	}
	if _, err := s.LinkSlack(ctx, codeHash("one"), "T09OTHER", "zsub1", now); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("a code links under another team: %v", err)
	}
	if got, err := s.LinkSlack(ctx, codeHash("one"), "T01ABC", "zsub1", now); err != nil || got.User != "U02DEF" {
		t.Fatalf("link: %+v %v", got, err)
	}
	if _, err := s.LinkSlack(ctx, codeHash("one"), "T01ABC", "zsub2", now); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("a used code links again: %v", err)
	}
	if sub, found, err := s.SlackLink(ctx, "T01ABC", "U02DEF"); err != nil || !found || sub != "zsub1" {
		t.Fatalf("the link: %q %v %v", sub, found, err)
	}
	// A second code replaces the first. A link is never moved to another account: that is
	// already_linked and leaves the code unused; the same account refreshes it; after an unlink the
	// other account may link.
	if err := s.MintSlackCode(ctx, codeHash("two"), c, now); err != nil {
		t.Fatal(err)
	}
	c.Handle = "dana.h"
	if err := s.MintSlackCode(ctx, codeHash("three"), c, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SlackCode(ctx, codeHash("two"), now); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("an earlier code of the same user survives a new one: %v", err)
	}
	if _, err := s.LinkSlack(ctx, codeHash("three"), "T01ABC", "zsub2", now); !errors.Is(err, ErrAlreadyLinked) {
		t.Fatalf("a link moved to another account: %v", err)
	}
	if sub, _, _ := s.SlackLink(ctx, "T01ABC", "U02DEF"); sub != "zsub1" {
		t.Fatalf("the refused move changed the link: %q", sub)
	}
	if got, err := s.LinkSlack(ctx, codeHash("three"), "T01ABC", "zsub1", now); err != nil || got.Handle != "dana.h" {
		t.Fatalf("the same account's refresh: %+v %v", got, err)
	}
	if links, _ := s.SlackLinksOf(ctx, "zsub1"); len(links) != 1 || links[0].Handle != "dana.h" || links[0].Name != "dana" {
		t.Fatalf("%+v", links)
	}
	if _, err := s.UnlinkSlack(ctx, "T01ABC", "U02DEF", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.MintSlackCode(ctx, codeHash("four"), c, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkSlack(ctx, codeHash("four"), "T01ABC", "zsub2", now); err != nil {
		t.Fatalf("after an unlink: %v", err)
	}
	if _, found, err := s.SlackLink(ctx, "T01ABC", "U09NONE"); err != nil || found {
		t.Fatalf("an unknown user is linked: %v %v", found, err)
	}
}

// An account lists its links and unlinks only its own; the App unlinks any; an account holds at
// most MaxSlackLinks Slack users, and a refused link leaves its code usable.
func TestSlackLinksBoundedAndUnlinked(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	now := time.Now().UTC()
	for i := range MaxSlackLinks + 1 {
		h := codeHash(fmt.Sprint("c", i))
		if err := s.MintSlackCode(ctx, h, SlackCode{Team: "T01ABC", User: fmt.Sprintf("U0%dX", i), Expires: now.Add(time.Minute)}, now); err != nil {
			t.Fatal(err)
		}
		_, err := s.LinkSlack(ctx, h, "T01ABC", "zsub", now)
		if i < MaxSlackLinks && err != nil {
			t.Fatal(err)
		}
		if i == MaxSlackLinks && !errors.Is(err, ErrTooManyLinks) {
			t.Fatalf("link %d past the bound: %v", i, err)
		}
	}
	links, err := s.SlackLinksOf(ctx, "zsub")
	if err != nil || len(links) != MaxSlackLinks {
		t.Fatalf("links of the account: %d %v", len(links), err)
	}
	if gone, err := s.UnlinkSlack(ctx, "T01ABC", "U00X", "someone-else"); err != nil || gone {
		t.Fatalf("another account unlinked: %v %v", gone, err)
	}
	if gone, err := s.UnlinkSlack(ctx, "T01ABC", "U00X", "zsub"); err != nil || !gone {
		t.Fatalf("the account's own unlink: %v %v", gone, err)
	}
	if gone, err := s.UnlinkSlack(ctx, "T01ABC", "U01X", ""); err != nil || !gone {
		t.Fatalf("the App's unlink: %v %v", gone, err)
	}
	// Room again: the refused code is still usable.
	if _, err := s.LinkSlack(ctx, codeHash(fmt.Sprint("c", MaxSlackLinks)), "T01ABC", "zsub", now); err != nil {
		t.Fatalf("the refused code after an unlink: %v", err)
	}
}

// The codes table is bounded: expired and used rows go at each mint, and past MaxSlackCodes live
// codes a mint is refused.
func TestSlackCodesBounded(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx, `INSERT INTO slack_link_codes (code_sha256, team_id, user_id, expires_at)
		SELECT sha256(i::text::bytea), 'T01ABC', 'U' || lpad(i::text, 4, '0'), $1 FROM generate_series(1, $2) i`, now.Add(time.Minute), MaxSlackCodes); err != nil {
		t.Fatal(err)
	}
	c := SlackCode{Team: "T01ABC", User: "U02NEW", Expires: now.Add(10 * time.Minute)}
	if err := s.MintSlackCode(ctx, codeHash("x"), c, now); !errors.Is(err, ErrTooManyCodes) {
		t.Fatalf("past the bound: %v", err)
	}
	// Two minutes on, every one of them has expired: the mint purges them and succeeds.
	if err := s.MintSlackCode(ctx, codeHash("x"), c, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM slack_link_codes`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows left: %d %v", n, err)
	}
}
