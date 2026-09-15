package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// TrustSeedOutcome is what applying one workspace-trust seed produced.
// Seeded is true both when the write landed and when the key already held
// true; Reason names why nothing was written when Seeded is false (profile
// config absent or unparsable — the launch still proceeds, with the
// interactive dialog as the surfaced fallback).
type TrustSeedOutcome struct {
	Seeded bool
	Reason string
}

// TrustSeeder is the consumer-owned port that applies a workspace-trust
// pre-seed to a harness profile configuration file: a locked, atomic
// read-modify-write of configPath setting
// projects[projectKey].hasTrustDialogAccepted = true via SeedTrustEdit.
// An absent or unparsable file is a not-seeded TrustSeedOutcome, never an
// error, and the file is then not created or rewritten. An error is a
// failure that must refuse the launch (the lock, read or write itself
// failed); its message never carries a path or an environment value.
// Implemented by internal/adapters/system.
type TrustSeeder interface {
	SeedWorkspaceTrust(ctx context.Context, configPath, projectKey string) (TrustSeedOutcome, error)
}

// trustDialogKey is the one per-project key of a Claude Code profile's
// .claude.json that suppresses the interactive workspace-trust dialog when
// true, verified against Claude Code 2.1.270
// (docs/architecture/native-harness-compat.md, "Workspace trust"). No other
// key is needed; an abandoned dialog persists a default entry carrying
// false, so a seed must overwrite false, never only add-if-absent.
const trustDialogKey = "hasTrustDialogAccepted"

// TrustSeedStep is the workspace-trust pre-seeding decision for one launch,
// computed by PlanTrustSeed: either the profile configuration file to seed
// and the exact projects key to set, or the reason no seed applies. The
// decision is evidence input only — it never gates the launch itself, and
// the interactive trust dialog remains the surfaced fallback whenever no
// seed lands.
type TrustSeedStep struct {
	// ConfigPath is the absolute path of the profile's .claude.json when
	// the launch seeds workspace trust; "" when it does not.
	ConfigPath string
	// ProjectKey is the projects-map key the seed sets: the caller-supplied
	// symlink-resolved absolute worktree path, exactly as the launched
	// harness will resolve its own working directory.
	ProjectKey string
	// Reason names why no seed applies when ConfigPath is ""; it never
	// carries an environment value or path.
	Reason string
}

// Seeds reports whether the step seeds a profile configuration file.
func (s TrustSeedStep) Seeds() bool { return s.ConfigPath != "" }

// PlanTrustSeed resolves the workspace-trust pre-seed for one launch from
// the launched harness, the SANITIZED environment the worker will exec
// with, and the symlink-resolved absolute worktree path that will be the
// worker's working directory. For Claude the trust file is
// $CLAUDE_CONFIG_DIR/.claude.json when the sanitized environment carries a
// config dir (a configured profile directory, or an explicit passthrough),
// else $HOME/.claude.json from that same environment. Codex is not seeded —
// its documented trust key sits behind login and is unverified — and
// opencode has no known workspace-trust mechanism
// (docs/architecture/native-harness-compat.md). The function is pure:
// resolution reads only its arguments, and a step it cannot resolve is a
// not-seeded reason, never an error — the interactive dialog fallback
// stays. Reasons name variables, never their values.
func PlanTrustSeed(harness string, sanitizedEnv []string, worktreePath string) TrustSeedStep {
	switch harness {
	case HarnessClaude:
	case HarnessCodex:
		return TrustSeedStep{Reason: "codex workspace trust is not seeded: its trust configuration sits behind login and is unverified"}
	case HarnessOpencode:
		return TrustSeedStep{Reason: "opencode workspace trust is not seeded: no workspace-trust mechanism is known for it"}
	default:
		return TrustSeedStep{Reason: fmt.Sprintf("harness %q has no workspace-trust seed", harness)}
	}
	if !strings.HasPrefix(worktreePath, "/") {
		return TrustSeedStep{Reason: "the worktree path is not absolute; no trust key can be composed"}
	}
	base := environValue(sanitizedEnv, "CLAUDE_CONFIG_DIR")
	if base == "" {
		base = environValue(sanitizedEnv, "HOME")
	}
	if base == "" {
		return TrustSeedStep{Reason: "the sanitized environment carries neither CLAUDE_CONFIG_DIR nor HOME; no profile configuration resolves"}
	}
	if !strings.HasPrefix(base, "/") {
		return TrustSeedStep{Reason: "the profile directory the sanitized environment names is not absolute; no profile configuration resolves"}
	}
	return TrustSeedStep{
		ConfigPath: strings.TrimSuffix(base, "/") + "/.claude.json",
		ProjectKey: worktreePath,
	}
}

// SeedTrustEdit computes the edit of a profile .claude.json that sets
// projects[projectKey].hasTrustDialogAccepted = true. The edit is
// byte-surgical: every byte outside the one replaced value (or the one
// insertion point) is preserved exactly — unknown fields, key order,
// whitespace and formatting included — because the document is never
// round-tripped through a decode/encode cycle. An inserted member is
// rendered compactly (no added whitespace) at the head of its enclosing
// object; that inserted text is the only formatting this function imposes.
//
// changed is false when the key already holds exactly true, so a caller
// can skip the write; edited is then nil. A document that is not valid
// JSON, whose top-level value is not an object, or whose "projects" value
// or matching project entry is not an object returns an error — the caller
// treats every error as "unparsable" and must not rewrite the file.
// Duplicate keys resolve to the LAST occurrence, matching what Claude
// Code's own JSON parse would read back.
func SeedTrustEdit(config []byte, projectKey string) (edited []byte, changed bool, err error) {
	loc, err := locateTrustSeed(config, projectKey)
	if err != nil {
		return nil, false, err
	}
	keyJSON, err := json.Marshal(projectKey)
	if err != nil {
		return nil, false, fmt.Errorf("encode project key: %w", err)
	}

	var site objectSite
	var insertion string
	switch {
	case loc.trustValue != nil:
		if loc.trustValue.isTrue {
			return nil, false, nil
		}
		return spliceBytes(config, loc.trustValue.start, loc.trustValue.end, "true"), true, nil
	case loc.entry != nil:
		site = *loc.entry
		insertion = `"` + trustDialogKey + `":true`
	case loc.projects != nil:
		site = *loc.projects
		insertion = string(keyJSON) + `:{"` + trustDialogKey + `":true}`
	default:
		site = loc.top
		insertion = `"projects":{` + string(keyJSON) + `:{"` + trustDialogKey + `":true}}`
	}
	if site.nonEmpty {
		insertion += ","
	}
	return spliceBytes(config, site.insertAt, site.insertAt, insertion), true, nil
}

// spliceBytes returns src with [start, end) replaced by text, copying into
// a fresh slice so src is never mutated.
func spliceBytes(src []byte, start, end int64, text string) []byte {
	out := make([]byte, 0, int64(len(src))-(end-start)+int64(len(text)))
	out = append(out, src[:start]...)
	out = append(out, text...)
	return append(out, src[end:]...)
}

// objectSite is one located JSON object: the byte offset just after its
// opening brace — a valid insertion point for a first member — and whether
// it already has members (an insertion then needs a trailing comma).
type objectSite struct {
	insertAt int64
	nonEmpty bool
}

// valueSite is one located member value: its exact byte range and whether
// it is the literal true.
type valueSite struct {
	start, end int64
	isTrue     bool
}

// trustSeedLocation is everything SeedTrustEdit needs from one walk of the
// document: the top-level object, the last "projects" member's object, the
// last matching project entry's object inside it, and the last
// hasTrustDialogAccepted member's value inside that. Each inner site is nil
// when its container exists but the member does not.
type trustSeedLocation struct {
	top        objectSite
	projects   *objectSite
	entry      *objectSite
	trustValue *valueSite
}

// trustSeedScanner walks one profile config's tokens while keeping the raw
// bytes at hand, so a member value's exact byte range can be recovered from
// token offsets.
type trustSeedScanner struct {
	dec *json.Decoder
	src []byte
}

// locateTrustSeed tokenizes config once and records the byte locations the
// seed edit targets. Any tokenizer error, trailing content after the
// top-level value, or a non-object where the seed path requires an object
// is returned as the "unparsable" error the caller maps to a not-seeded
// outcome.
func locateTrustSeed(config []byte, projectKey string) (trustSeedLocation, error) {
	s := &trustSeedScanner{dec: json.NewDecoder(bytes.NewReader(config)), src: config}
	s.dec.UseNumber()
	var loc trustSeedLocation

	top, err := s.dec.Token()
	if err != nil {
		return loc, fmt.Errorf("parse profile config: %w", err)
	}
	if top != json.Delim('{') {
		return loc, fmt.Errorf("profile config's top-level value is not an object")
	}
	loc.top = objectSite{insertAt: s.dec.InputOffset(), nonEmpty: s.dec.More()}

	for s.dec.More() {
		key, err := s.memberKey()
		if err != nil {
			return loc, err
		}
		if key != "projects" {
			if skipErr := s.skipValue(); skipErr != nil {
				return loc, skipErr
			}
			continue
		}
		projects, err := s.requireObject(`the "projects" value`)
		if err != nil {
			return loc, err
		}
		// A later duplicate "projects" key supersedes everything located
		// under an earlier one, matching last-occurrence-wins parsing.
		loc.projects, loc.entry, loc.trustValue = &projects, nil, nil
		if err := s.walkProjects(projectKey, &loc); err != nil {
			return loc, err
		}
	}
	if err := s.closeObject(); err != nil {
		return loc, err
	}
	if _, err := s.dec.Token(); err != io.EOF {
		return loc, fmt.Errorf("profile config carries content after its top-level value")
	}
	return loc, nil
}

// walkProjects consumes the members of an already-entered projects object,
// recording the last entry matching projectKey and, within it, the last
// hasTrustDialogAccepted value.
func (s *trustSeedScanner) walkProjects(projectKey string, loc *trustSeedLocation) error {
	for s.dec.More() {
		key, err := s.memberKey()
		if err != nil {
			return err
		}
		if key != projectKey {
			if skipErr := s.skipValue(); skipErr != nil {
				return skipErr
			}
			continue
		}
		entry, err := s.requireObject("the worktree's project entry")
		if err != nil {
			return err
		}
		loc.entry, loc.trustValue = &entry, nil
		if err := s.walkEntry(loc); err != nil {
			return err
		}
	}
	return s.closeObject()
}

// walkEntry consumes the members of an already-entered project entry,
// recording the last hasTrustDialogAccepted member's exact value bytes.
func (s *trustSeedScanner) walkEntry(loc *trustSeedLocation) error {
	for s.dec.More() {
		key, err := s.memberKey()
		if err != nil {
			return err
		}
		if key != trustDialogKey {
			if skipErr := s.skipValue(); skipErr != nil {
				return skipErr
			}
			continue
		}
		afterKey := s.dec.InputOffset()
		tok, err := s.dec.Token()
		if err != nil {
			return fmt.Errorf("parse profile config: %w", err)
		}
		if delim, ok := tok.(json.Delim); ok {
			if skipErr := s.skipOpenValue(delim); skipErr != nil {
				return skipErr
			}
		}
		value := valueSite{end: s.dec.InputOffset(), isTrue: tok == true}
		value.start, err = valueStartAfterColon(s.src, afterKey)
		if err != nil {
			return err
		}
		loc.trustValue = &value
	}
	return s.closeObject()
}

// memberKey reads one object member's key token.
func (s *trustSeedScanner) memberKey() (string, error) {
	tok, err := s.dec.Token()
	if err != nil {
		return "", fmt.Errorf("parse profile config: %w", err)
	}
	key, ok := tok.(string)
	if !ok {
		return "", fmt.Errorf("parse profile config: object member key is not a string")
	}
	return key, nil
}

// requireObject reads one member's value, requiring it to be an object,
// and returns its insertion site.
func (s *trustSeedScanner) requireObject(what string) (objectSite, error) {
	tok, err := s.dec.Token()
	if err != nil {
		return objectSite{}, fmt.Errorf("parse profile config: %w", err)
	}
	if tok != json.Delim('{') {
		return objectSite{}, fmt.Errorf("profile config: %s is not an object", what)
	}
	return objectSite{insertAt: s.dec.InputOffset(), nonEmpty: s.dec.More()}, nil
}

// closeObject consumes an object's closing brace after its members.
func (s *trustSeedScanner) closeObject() error {
	tok, err := s.dec.Token()
	if err != nil {
		return fmt.Errorf("parse profile config: %w", err)
	}
	if tok != json.Delim('}') {
		return fmt.Errorf("parse profile config: object did not close where expected")
	}
	return nil
}

// skipValue consumes one complete member value of any kind.
func (s *trustSeedScanner) skipValue() error {
	tok, err := s.dec.Token()
	if err != nil {
		return fmt.Errorf("parse profile config: %w", err)
	}
	if delim, ok := tok.(json.Delim); ok {
		return s.skipOpenValue(delim)
	}
	return nil
}

// skipOpenValue consumes the remainder of a composite value whose opening
// delimiter was already read.
func (s *trustSeedScanner) skipOpenValue(open json.Delim) error {
	if open != json.Delim('{') && open != json.Delim('[') {
		return fmt.Errorf("parse profile config: unexpected closing delimiter")
	}
	depth := 1
	for depth > 0 {
		tok, err := s.dec.Token()
		if err != nil {
			return fmt.Errorf("parse profile config: %w", err)
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}

// valueStartAfterColon scans src forward from the offset just after a
// member's key token — over the whitespace, the one colon, and the
// whitespace after it, exactly the bytes JSON grammar permits there — to
// the first byte of the member's value.
func valueStartAfterColon(src []byte, afterKey int64) (int64, error) {
	i := afterKey
	for i < int64(len(src)) && isJSONSpace(src[i]) {
		i++
	}
	if i >= int64(len(src)) || src[i] != ':' {
		return 0, fmt.Errorf("parse profile config: no colon after a member key")
	}
	i++
	for i < int64(len(src)) && isJSONSpace(src[i]) {
		i++
	}
	if i >= int64(len(src)) {
		return 0, fmt.Errorf("parse profile config: no value after a member key")
	}
	return i, nil
}

// isJSONSpace reports whether b is one of JSON's four whitespace bytes.
func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
