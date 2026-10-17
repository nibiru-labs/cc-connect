package a2a

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	rosterBodyLimit        = 4096
	rosterPayloadLimit     = 65536
	rosterChunkLimit       = 32
	rosterTransferTTL      = time.Hour
	rosterMaxActiveGroups  = 8
	rosterMaxStateBytes    = 2 << 20
	rosterMaxSeenIDs       = 4096
	rosterSeenIDTTL        = 24 * time.Hour
	rosterTombstoneTTL     = 30 * 24 * time.Hour
	rosterMaxTombstones    = 256
	rosterStateFormat      = 1
	rosterMaxJSONDepth     = 128
	rosterPayloadFormat    = "source-roster-json-utf8"
	rosterManifestType     = "xstation.roster.manifest.v1"
	rosterChunkType        = "xstation.roster.chunk.v1"
	rosterLegacyRosterType = "xstation.roster.v1"
	grokRosterAuthorityRef = "nibiru#1301 comment6233496788"
)

var (
	rosterHashPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	rosterNamePattern      = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	rosterFullNamePattern  = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	rosterRetiredDateRegex = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

type rosterReceiveState string

const (
	rosterReceivePending   rosterReceiveState = "pending"
	rosterReceiveDuplicate rosterReceiveState = "duplicate"
	rosterReceiveRejected  rosterReceiveState = "rejected"
	rosterReceiveReady     rosterReceiveState = "ready"
)

type rosterReceiveResult struct {
	state     rosterReceiveState
	key       string
	payload   string
	reason    string
	retryable bool
}

type rosterPiece struct {
	kind            string
	version         int64
	issuer          string
	hash            string
	total           int
	index           int
	text            string
	payloadSize     int64
	body            string
	approvedBy      string
	authorityRef    string
	hasAuthorityRef bool
}

type rosterReceiver struct {
	expectedIssuer string
	configErr      error
	store          *rosterTransferStore
}

func newRosterReceiver(opts map[string]any, allow map[string]bool) *rosterReceiver {
	r := &rosterReceiver{}
	if raw, exists := opts["roster_issuer"]; exists {
		issuer, ok := raw.(string)
		if !ok {
			r.configErr = errors.New("roster_issuer must be a string")
		} else {
			r.expectedIssuer = strings.TrimSpace(issuer)
			if r.expectedIssuer == "" || strings.ContainsRune(r.expectedIssuer, '\x00') {
				r.configErr = errors.New("roster_issuer must be non-empty")
			} else if !allow[strings.ToLower(r.expectedIssuer)] {
				r.configErr = errors.New("roster_issuer must already be present in allow_from")
			}
		}
	}
	statePath := ""
	if r.expectedIssuer != "" {
		if cacheDir, err := os.UserCacheDir(); err == nil {
			issuerHash := sha256.Sum256([]byte(r.expectedIssuer))
			statePath = filepath.Join(cacheDir, "cc-connect", fmt.Sprintf("a2a-roster-transfers-v1-%x.json", issuerHash[:8]))
		}
	}
	r.store = newRosterTransferStore(statePath, time.Now)
	return r
}

func (r *rosterReceiver) isTransferMessage(text string) bool {
	if r == nil {
		return false
	}
	typ, ok := sniffRosterType(text)
	return ok && (typ == rosterManifestType || typ == rosterChunkType)
}

func (r *rosterReceiver) accept(message Inbound) rosterReceiveResult {
	if r.configErr != nil {
		return rosterReceiveResult{state: rosterReceiveRejected, reason: r.configErr.Error()}
	}
	if r.expectedIssuer == "" {
		return rosterReceiveResult{state: rosterReceiveRejected, reason: "roster_issuer is not configured"}
	}
	if message.SenderID != r.expectedIssuer {
		return rosterReceiveResult{state: rosterReceiveRejected, reason: "authenticated sender does not match trusted roster_issuer"}
	}
	piece, err := parseRosterPiece(message.Text)
	if err != nil {
		return rosterReceiveResult{state: rosterReceiveRejected, reason: err.Error()}
	}
	if piece.issuer != message.SenderID || piece.issuer != r.expectedIssuer {
		return rosterReceiveResult{state: rosterReceiveRejected, reason: "issued_by does not match authenticated sender and expected_issuer"}
	}
	if message.MessageID == "" || len(message.MessageID) > 256 {
		return rosterReceiveResult{state: rosterReceiveRejected, reason: "invalid transport message ID"}
	}
	return r.store.add(message.MessageID, message.SenderID, r.expectedIssuer, piece)
}

func (r *rosterReceiver) markDelivered(key, messageID string) error {
	if r == nil || r.store == nil {
		return errors.New("roster receiver is unavailable")
	}
	return r.store.markDelivered(key, messageID)
}

// sniffRosterType examines only a root object's type field. It returns a type
// as soon as one is seen so a truncated, tagged transfer envelope is rejected
// instead of being handed to the normal model path.
func sniffRosterType(text string) (string, bool) {
	probe := text
	if len(probe) > rosterBodyLimit {
		probe = probe[:rosterBodyLimit]
	}
	d := json.NewDecoder(strings.NewReader(probe))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return "", false
	}
	found := ""
	for d.More() {
		token, err := d.Token()
		if err != nil {
			break
		}
		key, ok := token.(string)
		if !ok {
			break
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			break
		}
		if key == "type" {
			var typ string
			if json.Unmarshal(raw, &typ) == nil && (typ == rosterManifestType || typ == rosterChunkType) {
				found = typ
			}
		}
	}
	// The type may be a later root field in an oversized envelope, with its
	// value encoded using JSON escapes. Inspect the root object directly.
	if len(text) > rosterBodyLimit {
		return sniffLargeRosterRootType(text)
	}
	return found, found != ""
}

func sniffLargeRosterRootType(text string) (string, bool) {
	d := json.NewDecoder(strings.NewReader(text))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return "", false
	}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return "", false
		}
		key, ok := token.(string)
		if !ok {
			return "", false
		}
		if key == "type" {
			value, err := d.Token()
			if err != nil {
				return "", false
			}
			if typ, ok := value.(string); ok && (typ == rosterManifestType || typ == rosterChunkType) {
				return typ, true
			}
			if err := skipJSONValueAfterToken(d, value); err != nil {
				return "", false
			}
			continue
		}
		if err := skipJSONValue(d); err != nil {
			return "", false
		}
	}
	return "", false
}

// skipJSONValue consumes one value from d. It is iterative so deeply nested
// ordinary JSON cannot grow the Go call stack during type sniffing.
func skipJSONValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	return skipJSONValueAfterToken(d, token)
}

func skipJSONValueAfterToken(d *json.Decoder, token any) error {
	delim, ok := token.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil
	}
	stack := []json.Delim{delim}
	for len(stack) > 0 {
		token, err := d.Token()
		if err != nil {
			return err
		}
		if child, ok := token.(json.Delim); ok {
			switch child {
			case '{', '[':
				stack = append(stack, child)
			case '}', ']':
				stack = stack[:len(stack)-1]
			}
		}
	}
	return nil
}

func validateRosterApprovalMetadata(value map[string]any) error {
	approver, _ := value["approved_by"].(string)
	authorityRef, hasAuthorityRef := value["authority_ref"]
	if approver == "user" && !hasAuthorityRef {
		return nil
	}
	if approver == "grok" && authorityRef == grokRosterAuthorityRef {
		return nil
	}
	return errors.New("roster approved_by / authority_ref combination is not supported")
}

func parseRosterPiece(body string) (rosterPiece, error) {
	if len(body) > rosterBodyLimit {
		return rosterPiece{}, fmt.Errorf("roster message exceeds %d bytes", rosterBodyLimit)
	}
	if !utf8.ValidString(body) {
		return rosterPiece{}, errors.New("roster envelope is not valid UTF-8")
	}
	value, err := decodeUniqueJSON([]byte(body))
	if err != nil {
		return rosterPiece{}, fmt.Errorf("invalid roster envelope JSON: %w", err)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return rosterPiece{}, errors.New("roster envelope must be a JSON object")
	}
	typ, ok := obj["type"].(string)
	if !ok || (typ != rosterManifestType && typ != rosterChunkType) {
		return rosterPiece{}, errors.New("unsupported roster transfer type")
	}
	version, err := positiveJSONInteger(obj["version"], "version")
	if err != nil {
		return rosterPiece{}, err
	}
	issuer, ok := obj["issued_by"].(string)
	if !ok || issuer == "" || strings.ContainsRune(issuer, '\x00') {
		return rosterPiece{}, errors.New("issued_by must be a non-empty string")
	}
	sourceHash, ok := obj["source_sha256"].(string)
	if !ok || !rosterHashPattern.MatchString(sourceHash) {
		return rosterPiece{}, errors.New("source_sha256 must be a lowercase SHA-256")
	}
	payloadHash, ok := obj["payload_sha256"].(string)
	if !ok || payloadHash != sourceHash {
		return rosterPiece{}, errors.New("source_sha256 and payload_sha256 must match")
	}
	piece := rosterPiece{kind: typ, version: version, issuer: issuer, hash: sourceHash, body: body}
	if typ == rosterManifestType {
		if err := validateRosterApprovalMetadata(obj); err != nil {
			return rosterPiece{}, fmt.Errorf("invalid manifest approval metadata: %w", err)
		}
		piece.approvedBy, _ = obj["approved_by"].(string)
		piece.authorityRef, piece.hasAuthorityRef = obj["authority_ref"].(string)
		if obj["payload_format"] != rosterPayloadFormat {
			return rosterPiece{}, errors.New("unsupported roster payload_format")
		}
		payloadSize, err := boundedJSONInteger(obj["payload_bytes"], 1, rosterPayloadLimit, "payload_bytes")
		if err != nil {
			return rosterPiece{}, err
		}
		total, err := boundedJSONInteger(obj["chunks"], 1, rosterChunkLimit, "chunks")
		if err != nil {
			return rosterPiece{}, err
		}
		piece.payloadSize = payloadSize
		piece.total = int(total)
		return piece, nil
	}
	index, err := boundedJSONInteger(obj["index"], 0, rosterChunkLimit-1, "index")
	if err != nil {
		return rosterPiece{}, err
	}
	total, err := boundedJSONInteger(obj["total"], 1, rosterChunkLimit, "total")
	if err != nil {
		return rosterPiece{}, err
	}
	if index >= total {
		return rosterPiece{}, errors.New("chunk index must be less than total")
	}
	text, ok := obj["text"].(string)
	if !ok || text == "" || !utf8.ValidString(text) {
		return rosterPiece{}, errors.New("chunk text must be non-empty valid UTF-8")
	}
	piece.index = int(index)
	piece.total = int(total)
	piece.text = text
	return piece, nil
}

func decodeUniqueJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeUniqueValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, errors.New("trailing JSON value")
		}
		return nil, err
	}
	return value, nil
}

func decodeUniqueValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > rosterMaxJSONDepth {
		return nil, errors.New("JSON nesting exceeds the receiver limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}
	switch delim {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("JSON object key is not a string")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("duplicate JSON field %q", key)
			}
			value, err := decodeUniqueValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("unterminated JSON object")
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			value, err := decodeUniqueValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("unterminated JSON array")
		}
		return array, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

func positiveJSONInteger(value any, name string) (int64, error) {
	return boundedJSONInteger(value, 1, int64(^uint64(0)>>1), name)
}

func boundedJSONInteger(value any, min, max int64, name string) (int64, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	parsed, err := strconv.ParseInt(string(number), 10, 64)
	if err != nil || parsed < min || parsed > max {
		return 0, fmt.Errorf("%s must be an integer in range %d..%d", name, min, max)
	}
	return parsed, nil
}

type rosterTransferDiskState struct {
	FormatVersion int                             `json:"format_version"`
	HighWater     *rosterTransferHighWater        `json:"high_water,omitempty"`
	Groups        map[string]*rosterTransferGroup `json:"groups"`
	Tombstones    map[string]time.Time            `json:"tombstones"`
	SeenMessages  map[string]rosterSeenMessage    `json:"seen_messages"`
}

type rosterTransferHighWater struct {
	Version int64  `json:"version"`
	Hash    string `json:"hash"`
	State   string `json:"state"`
}

type rosterTransferGroup struct {
	Version           int64                     `json:"version"`
	Hash              string                    `json:"hash"`
	Total             int                       `json:"total"`
	FirstSeen         time.Time                 `json:"first_seen"`
	Status            string                    `json:"status"`
	ManifestBody      string                    `json:"manifest_body,omitempty"`
	ManifestMessageID string                    `json:"manifest_message_id,omitempty"`
	Chunks            map[int]rosterStoredChunk `json:"chunks,omitempty"`
	FinalMessageID    string                    `json:"final_message_id,omitempty"`
	Payload           string                    `json:"payload,omitempty"`
}

type rosterStoredChunk struct {
	Body      string `json:"body"`
	MessageID string `json:"message_id"`
}

type rosterSeenMessage struct {
	Key  string    `json:"key"`
	Seen time.Time `json:"seen"`
}

type rosterTransferStore struct {
	mu      sync.Mutex
	path    string
	now     func() time.Time
	loaded  bool
	loadErr error
	state   rosterTransferDiskState
}

func newRosterTransferStore(path string, now func() time.Time) *rosterTransferStore {
	if now == nil {
		now = time.Now
	}
	if path == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return &rosterTransferStore{now: now, loaded: true, loadErr: fmt.Errorf("resolve user cache directory: %w", err)}
		}
		path = filepath.Join(cacheDir, "cc-connect", "a2a-roster-transfers-v1.json")
	}
	return &rosterTransferStore{path: path, now: now}
}

func emptyRosterTransferState() rosterTransferDiskState {
	return rosterTransferDiskState{
		FormatVersion: rosterStateFormat,
		Groups:        map[string]*rosterTransferGroup{},
		Tombstones:    map[string]time.Time{},
		SeenMessages:  map[string]rosterSeenMessage{},
	}
}

func (s *rosterTransferStore) add(messageID, sender, expectedIssuer string, piece rosterPiece) rosterReceiveResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return rosterReceiveResult{state: rosterReceiveRejected, reason: "load durable roster state: " + err.Error(), retryable: true}
	}
	next, err := cloneRosterTransferState(s.state)
	if err != nil {
		return rosterReceiveResult{state: rosterReceiveRejected, reason: "clone durable roster state: " + err.Error(), retryable: true}
	}
	now := s.now().UTC()
	pruneRosterTransferState(&next, now)
	key := rosterTransferKey(sender, piece.version, piece.hash)

	if seen, ok := next.SeenMessages[messageID]; ok {
		return s.replaySeenMessage(&next, messageID, sender, expectedIssuer, key, seen, now)
	}

	if result, handled := s.rejectKnownRoster(&next, messageID, key, piece, now); handled {
		return result
	}
	group, result, handled := s.prepareRosterGroup(&next, messageID, key, piece, now)
	if handled {
		return result
	}
	next.SeenMessages[messageID] = rosterSeenMessage{Key: key, Seen: now}
	if reason := addRosterPiece(&next, key, group, messageID, piece, now); reason != "" {
		return s.saveRosterResult(&next, rosterReceiveResult{state: rosterReceiveRejected, key: key, reason: reason}, "persist invalid roster rejection: ")
	}
	return s.finishRosterPiece(&next, sender, expectedIssuer, messageID, key, group, piece, now)
}

func (s *rosterTransferStore) replaySeenMessage(next *rosterTransferDiskState, messageID, sender, expectedIssuer, key string, seen rosterSeenMessage, now time.Time) rosterReceiveResult {
	if seen.Key == key {
		if group := next.Groups[key]; group != nil && group.Status == "ready" && group.FinalMessageID == messageID {
			payload, err := reassembleRosterTransfer(sender, expectedIssuer, group.ManifestBody, group.Chunks)
			if err != nil || string(payload) != group.Payload {
				rejectRosterGroup(next, key, now)
				reason := "ready roster payload differs from its validated chunks"
				if err != nil {
					reason = "ready roster failed revalidation: " + err.Error()
				}
				return s.saveRosterResult(next, rosterReceiveResult{state: rosterReceiveRejected, key: key, reason: reason}, "persist corrupt ready roster rejection: ")
			}
			return s.saveRosterResult(next, rosterReceiveResult{state: rosterReceiveReady, key: key, payload: string(payload)}, "persist roster retry state: ")
		}
	}
	return s.saveRosterResult(next, rosterReceiveResult{state: rosterReceiveDuplicate, key: key,
		reason: "transport message ID was already recorded"}, "persist duplicate roster state: ")
}

func (s *rosterTransferStore) rejectKnownRoster(next *rosterTransferDiskState, messageID, key string, piece rosterPiece, now time.Time) (rosterReceiveResult, bool) {
	if expiry, ok := next.Tombstones[key]; ok && now.Before(expiry) {
		next.SeenMessages[messageID] = rosterSeenMessage{Key: key, Seen: now}
		result := rosterReceiveResult{state: rosterReceiveRejected, key: key, reason: "roster transfer was expired or rejected"}
		return s.saveRosterResult(next, result, "persist expired roster rejection: "), true
	}
	if reason := checkRosterHighWater(next.HighWater, piece.version, piece.hash); reason != "" {
		next.SeenMessages[messageID] = rosterSeenMessage{Key: key, Seen: now}
		addRosterTombstone(next, key, now)
		result := rosterReceiveResult{state: rosterReceiveRejected, key: key, reason: reason}
		return s.saveRosterResult(next, result, "persist stale roster rejection: "), true
	}
	return rosterReceiveResult{}, false
}

func (s *rosterTransferStore) prepareRosterGroup(next *rosterTransferDiskState, messageID, key string, piece rosterPiece, now time.Time) (*rosterTransferGroup, rosterReceiveResult, bool) {
	if group := next.Groups[key]; group != nil && group.Status == "ready" {
		next.SeenMessages[messageID] = rosterSeenMessage{Key: key, Seen: now}
		result := rosterReceiveResult{state: rosterReceiveDuplicate, key: key, reason: "roster transfer is already awaiting its turn result"}
		return nil, s.saveRosterResult(next, result, "persist duplicate completed roster: "), true
	}
	if group := next.Groups[key]; group != nil {
		return group, rosterReceiveResult{}, false
	}
	if len(next.Groups) >= rosterMaxActiveGroups {
		next.SeenMessages[messageID] = rosterSeenMessage{Key: key, Seen: now}
		addRosterTombstone(next, key, now)
		result := rosterReceiveResult{state: rosterReceiveRejected, key: key, reason: "too many active roster transfers"}
		return nil, s.saveRosterResult(next, result, "persist roster capacity rejection: "), true
	}
	group := &rosterTransferGroup{Version: piece.version, Hash: piece.hash, FirstSeen: now,
		Status: "collecting", Chunks: map[int]rosterStoredChunk{}}
	next.Groups[key] = group
	return group, rosterReceiveResult{}, false
}

func addRosterPiece(next *rosterTransferDiskState, key string, group *rosterTransferGroup, messageID string, piece rosterPiece, now time.Time) string {
	if piece.kind == rosterManifestType {
		if reason := advanceRosterHighWater(next, piece, now); reason != "" {
			addRosterTombstone(next, key, now)
			delete(next.Groups, key)
			return reason
		}
		if group.ManifestBody != "" {
			rejectRosterGroup(next, key, now)
			return "duplicate roster manifest"
		}
		if group.Total != 0 && group.Total != piece.total {
			rejectRosterGroup(next, key, now)
			return "manifest chunk count conflicts with stored chunks"
		}
		group.Total = piece.total
		group.ManifestBody = piece.body
		group.ManifestMessageID = messageID
		return ""
	}
	if group.Total != 0 && group.Total != piece.total {
		rejectRosterGroup(next, key, now)
		return "chunk total conflicts with stored transfer"
	}
	if _, exists := group.Chunks[piece.index]; exists {
		rejectRosterGroup(next, key, now)
		return "duplicate roster chunk index"
	}
	group.Total = piece.total
	group.Chunks[piece.index] = rosterStoredChunk{Body: piece.body, MessageID: messageID}
	if storedRosterTextBytes(group.Chunks) > rosterPayloadLimit {
		rejectRosterGroup(next, key, now)
		return "stored roster chunks exceed 65,536 bytes"
	}
	return ""
}

func (s *rosterTransferStore) finishRosterPiece(next *rosterTransferDiskState, sender, expectedIssuer, messageID, key string, group *rosterTransferGroup, piece rosterPiece, now time.Time) rosterReceiveResult {
	if group.ManifestBody == "" || len(group.Chunks) != group.Total {
		return s.saveRosterResult(next, rosterReceiveResult{state: rosterReceivePending, key: key}, "persist partial roster before ACK: ")
	}
	payload, err := reassembleRosterTransfer(sender, expectedIssuer, group.ManifestBody, group.Chunks)
	if err != nil {
		rejectRosterGroup(next, key, now)
		return s.saveRosterResult(next, rosterReceiveResult{state: rosterReceiveRejected, key: key, reason: err.Error()}, "persist invalid roster rejection: ")
	}
	group.Status = "ready"
	group.Payload = string(payload)
	group.FinalMessageID = messageID
	if next.HighWater != nil && next.HighWater.Version == piece.version && next.HighWater.Hash == piece.hash {
		next.HighWater.State = "ready"
	}
	return s.saveRosterResult(next, rosterReceiveResult{state: rosterReceiveReady, key: key, payload: group.Payload}, "persist completed roster before delivery: ")
}

func (s *rosterTransferStore) saveRosterResult(next *rosterTransferDiskState, result rosterReceiveResult, errorContext string) rosterReceiveResult {
	if err := s.commitLocked(*next); err != nil {
		return rosterReceiveResult{state: rosterReceiveRejected, key: result.key,
			reason: errorContext + err.Error(), retryable: true}
	}
	return result
}

func (s *rosterTransferStore) markDelivered(key, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	next, err := cloneRosterTransferState(s.state)
	if err != nil {
		return err
	}
	group := next.Groups[key]
	if group == nil {
		if next.HighWater != nil && next.HighWater.State == "delivered" {
			_, version, hash, ok := parseRosterTransferKey(key)
			if ok && version == next.HighWater.Version && hash == next.HighWater.Hash {
				return nil
			}
		}
		return errors.New("completed roster transfer is missing from durable state")
	}
	if group.Status != "ready" || group.FinalMessageID != messageID {
		return errors.New("roster turn does not match the ready transfer")
	}
	if next.HighWater != nil && next.HighWater.Version == group.Version && next.HighWater.Hash == group.Hash {
		next.HighWater.State = "delivered"
	}
	delete(next.Groups, key)
	return s.commitLocked(next)
}

func reassembleRosterTransfer(authenticatedSender, expectedIssuer, manifestBody string, stored map[int]rosterStoredChunk) ([]byte, error) {
	if authenticatedSender == "" || expectedIssuer == "" || authenticatedSender != expectedIssuer {
		return nil, errors.New("authenticated sender does not match trusted expected_issuer")
	}
	manifest, err := parseRosterPiece(manifestBody)
	if err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	if manifest.kind != rosterManifestType || manifest.issuer != authenticatedSender || manifest.issuer != expectedIssuer {
		return nil, errors.New("manifest issued_by does not match authenticated sender and expected_issuer")
	}
	if len(stored) != manifest.total {
		return nil, errors.New("roster chunks are incomplete")
	}
	parts := make([]string, manifest.total)
	for index := 0; index < manifest.total; index++ {
		storedChunk, ok := stored[index]
		if !ok {
			return nil, fmt.Errorf("missing roster chunk %d", index)
		}
		chunk, err := parseRosterPiece(storedChunk.Body)
		if err != nil {
			return nil, fmt.Errorf("invalid chunk %d: %w", index, err)
		}
		if chunk.kind != rosterChunkType || chunk.issuer != authenticatedSender || chunk.issuer != expectedIssuer {
			return nil, fmt.Errorf("chunk %d issued_by does not match authenticated sender and expected_issuer", index)
		}
		if chunk.version != manifest.version || chunk.hash != manifest.hash || chunk.total != manifest.total || chunk.index != index {
			return nil, fmt.Errorf("chunk %d metadata does not match manifest", index)
		}
		parts[index] = chunk.text
	}
	joined := strings.Join(parts, "")
	if !utf8.ValidString(joined) {
		return nil, errors.New("reassembled roster source is not valid UTF-8")
	}
	payload := []byte(joined)
	if int64(len(payload)) != manifest.payloadSize {
		return nil, errors.New("reassembled roster length does not match payload_bytes")
	}
	if len(payload) > rosterPayloadLimit {
		return nil, errors.New("reassembled roster exceeds 65,536 bytes")
	}
	digest := sha256.Sum256(payload)
	actualHash := hex.EncodeToString(digest[:])
	if actualHash != manifest.hash {
		return nil, errors.New("reassembled roster SHA-256 does not match manifest")
	}
	value, err := decodeUniqueJSON(payload)
	if err != nil {
		return nil, fmt.Errorf("reassembled roster is not valid UTF-8 JSON: %w", err)
	}
	roster, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("reassembled roster must be a JSON object")
	}
	version, err := positiveJSONInteger(roster["version"], "roster version")
	if err != nil {
		return nil, err
	}
	approvedBy, _ := roster["approved_by"].(string)
	if roster["type"] != rosterLegacyRosterType || version != manifest.version || roster["issued_by"] != manifest.issuer || approvedBy != manifest.approvedBy {
		return nil, errors.New("reconstructed roster header does not match manifest")
	}
	if err := validateRosterApprovalMetadata(roster); err != nil {
		return nil, fmt.Errorf("reconstructed roster has invalid approval metadata: %w", err)
	}
	authorityRef, hasAuthorityRef := roster["authority_ref"]
	if hasAuthorityRef != manifest.hasAuthorityRef || (hasAuthorityRef && authorityRef != manifest.authorityRef) {
		return nil, errors.New("reconstructed roster authority_ref does not match manifest")
	}
	manifestCharter, manifestHasCharter := manifestEnvelopeCharter(manifestBody)
	rosterCharter, rosterHasCharter := roster["charter"]
	if manifestHasCharter != rosterHasCharter || (manifestHasCharter && !reflect.DeepEqual(manifestCharter, rosterCharter)) {
		return nil, errors.New("reconstructed roster charter does not exactly match manifest")
	}
	if err := validateRoster(roster, expectedIssuer); err != nil {
		return nil, fmt.Errorf("reconstructed roster failed validation: %w", err)
	}
	return payload, nil
}

func manifestEnvelopeCharter(body string) (any, bool) {
	value, err := decodeUniqueJSON([]byte(body))
	if err != nil {
		return nil, false
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	charter, exists := obj["charter"]
	return charter, exists
}

func validateRoster(roster map[string]any, expectedIssuer string) error {
	if roster["type"] != rosterLegacyRosterType {
		return errors.New("type must be xstation.roster.v1")
	}
	if err := validateRosterApprovalMetadata(roster); err != nil {
		return err
	}
	if _, err := positiveJSONInteger(roster["version"], "version"); err != nil {
		return err
	}
	if issuer, ok := roster["issued_by"].(string); !ok || issuer == "" || issuer != expectedIssuer {
		return errors.New("issued_by must match trusted expected_issuer")
	}
	members, ok := roster["members"].([]any)
	if !ok {
		return errors.New("members must be an array")
	}
	memberIDs, memberFullNames, err := validateRosterMembers(members)
	if err != nil {
		return err
	}
	if raw, exists := roster["charter"]; exists {
		if err := validateRosterCharter(raw); err != nil {
			return err
		}
	}
	if raw, exists := roster["retired"]; exists {
		if err := validateRosterRetired(raw, memberIDs, memberFullNames); err != nil {
			return err
		}
	}
	return nil
}

func validateRosterMembers(members []any) (map[string]bool, map[string]bool, error) {
	memberIDs := map[string]bool{}
	memberNames := map[string]bool{}
	memberFullNames := map[string]bool{}
	memberByID := make([]map[string]any, 0, len(members))
	directIDs := map[string]bool{}
	for _, value := range members {
		member, ok := value.(map[string]any)
		if !ok {
			return nil, nil, errors.New("members must contain JSON objects")
		}
		id, idOK := member["id"].(string)
		name, nameOK := member["name"].(string)
		fullName, fullNameOK := member["full_name"].(string)
		if !idOK || id == "" || !nameOK || !rosterNamePattern.MatchString(name) ||
			!fullNameOK || !rosterFullNamePattern.MatchString(fullName) ||
			!strings.HasPrefix(fullName, name+"-") || len(fullName) == len(name)+1 {
			return nil, nil, errors.New("member id/name/full_name is invalid")
		}
		if memberIDs[id] || memberNames[name] || memberFullNames[fullName] {
			return nil, nil, errors.New("member id, name, and full_name must be unique")
		}
		memberIDs[id], memberNames[name], memberFullNames[fullName] = true, true, true
		status := "active"
		if raw, exists := member["status"]; exists {
			var valid bool
			status, valid = raw.(string)
			if !valid || (status != "active" && status != "pending_join" && status != "sealed") {
				return nil, nil, errors.New("member status is invalid")
			}
		}
		for _, flag := range []string{"optional", "service"} {
			if raw, exists := member[flag]; exists {
				if _, valid := raw.(bool); !valid {
					return nil, nil, fmt.Errorf("member %s must be boolean", flag)
				}
			}
		}
		if status == "active" {
			if _, hasRoute := member["route"]; !hasRoute {
				directIDs[id] = true
			}
		}
		memberByID = append(memberByID, member)
	}
	for _, member := range memberByID {
		if raw, exists := member["route"]; exists {
			route, ok := raw.(string)
			via := strings.TrimPrefix(route, "via:")
			if !ok || !strings.HasPrefix(route, "via:") || via == "" || via == member["id"] || !directIDs[via] {
				return nil, nil, errors.New("member route must name another direct active member")
			}
		}
	}
	return memberIDs, memberFullNames, nil
}

func validateRosterCharter(raw any) error {
	charter, ok := raw.(map[string]any)
	if !ok || len(charter) != 2 {
		return errors.New("charter must contain exactly version and sha256")
	}
	if _, err := positiveJSONInteger(charter["version"], "charter version"); err != nil {
		return err
	}
	hash, ok := charter["sha256"].(string)
	if !ok || !rosterHashPattern.MatchString(hash) {
		return errors.New("charter sha256 must be a lowercase SHA-256")
	}
	return nil
}

func validateRosterRetired(raw any, memberIDs, memberFullNames map[string]bool) error {
	retired, ok := raw.([]any)
	if !ok {
		return errors.New("retired must be an array")
	}
	for _, value := range retired {
		item, ok := value.(map[string]any)
		if !ok {
			return errors.New("retired must contain JSON objects")
		}
		for _, key := range []string{"id", "name", "full_name", "retired_at", "reason", "decision"} {
			text, ok := item[key].(string)
			if !ok || text == "" {
				return errors.New("retired item requires non-empty id/name/full_name/retired_at/reason/decision")
			}
		}
		id := item["id"].(string)
		name := item["name"].(string)
		fullName := item["full_name"].(string)
		retiredAt := item["retired_at"].(string)
		if !rosterRetiredDateRegex.MatchString(retiredAt) || !strings.HasPrefix(fullName, name+"-") || len(fullName) == len(name)+1 {
			return errors.New("retired item retired_at/full_name is invalid")
		}
		if memberIDs[id] || memberFullNames[fullName] {
			return errors.New("retired id/full_name conflicts with another roster entry")
		}
		memberIDs[id], memberFullNames[fullName] = true, true
	}
	return nil
}

func rosterTransferKey(sender string, version int64, hash string) string {
	return fmt.Sprintf("%s\x00%d\x00%s", sender, version, hash)
}

func checkRosterHighWater(high *rosterTransferHighWater, version int64, hash string) string {
	if high == nil {
		return ""
	}
	if version < high.Version {
		return "roster version is older than the persisted high-water mark"
	}
	if version == high.Version && hash != high.Hash {
		return "different roster payloads use the same version"
	}
	if version == high.Version && (high.State == "delivered" || high.State == "expired" || high.State == "rejected") {
		return "roster transfer for this version is already terminal"
	}
	return ""
}

func advanceRosterHighWater(state *rosterTransferDiskState, piece rosterPiece, now time.Time) string {
	if state.HighWater != nil && piece.version == state.HighWater.Version && piece.hash != state.HighWater.Hash {
		return "different roster payloads use the same version"
	}
	if state.HighWater != nil && piece.version < state.HighWater.Version {
		return "manifest version is older than the persisted high-water mark"
	}
	if state.HighWater != nil && piece.version == state.HighWater.Version &&
		(state.HighWater.State == "delivered" || state.HighWater.State == "expired" || state.HighWater.State == "rejected") {
		return "roster transfer for this version is already terminal"
	}
	if state.HighWater == nil || piece.version > state.HighWater.Version {
		for existingKey, group := range state.Groups {
			if group.Version < piece.version && group.Status != "ready" {
				addRosterTombstone(state, existingKey, now)
				delete(state.Groups, existingKey)
			}
		}
		state.HighWater = &rosterTransferHighWater{Version: piece.version, Hash: piece.hash, State: "collecting"}
	}
	if state.HighWater.Version == piece.version && state.HighWater.Hash == piece.hash && state.HighWater.State == "" {
		state.HighWater.State = "collecting"
	}
	return ""
}

func parseRosterTransferKey(key string) (string, int64, string, bool) {
	first := strings.IndexByte(key, '\x00')
	last := strings.LastIndexByte(key, '\x00')
	if first <= 0 || last <= first {
		return "", 0, "", false
	}
	sender := key[:first]
	version, err := strconv.ParseInt(key[first+1:last], 10, 64)
	if err != nil {
		return "", 0, "", false
	}
	hash := key[last+1:]
	return sender, version, hash, version > 0 && rosterHashPattern.MatchString(hash)
}

func rejectRosterGroup(state *rosterTransferDiskState, key string, now time.Time) {
	addRosterTombstone(state, key, now)
	if group := state.Groups[key]; group != nil && state.HighWater != nil &&
		state.HighWater.Version == group.Version && state.HighWater.Hash == group.Hash {
		state.HighWater.State = "rejected"
	}
	delete(state.Groups, key)
}

func addRosterTombstone(state *rosterTransferDiskState, key string, now time.Time) {
	state.Tombstones[key] = now.Add(rosterTombstoneTTL)
	if len(state.Tombstones) <= rosterMaxTombstones {
		return
	}
	type item struct {
		key string
		exp time.Time
	}
	items := make([]item, 0, len(state.Tombstones))
	for k, exp := range state.Tombstones {
		items = append(items, item{key: k, exp: exp})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].exp.Before(items[j].exp) })
	for _, stale := range items[:len(items)-rosterMaxTombstones] {
		delete(state.Tombstones, stale.key)
	}
}

func pruneRosterTransferState(state *rosterTransferDiskState, now time.Time) {
	for key, group := range state.Groups {
		if group.Status == "collecting" && now.Sub(group.FirstSeen) > rosterTransferTTL {
			addRosterTombstone(state, key, now)
			delete(state.Groups, key)
			if state.HighWater != nil && state.HighWater.Version == group.Version && state.HighWater.Hash == group.Hash && state.HighWater.State == "collecting" {
				state.HighWater.State = "expired"
			}
		}
	}
	for key, expiry := range state.Tombstones {
		if !now.Before(expiry) {
			delete(state.Tombstones, key)
		}
	}
	for id, message := range state.SeenMessages {
		if now.Sub(message.Seen) > rosterSeenIDTTL {
			delete(state.SeenMessages, id)
		}
	}
	if len(state.SeenMessages) > rosterMaxSeenIDs {
		type item struct {
			id   string
			time time.Time
		}
		items := make([]item, 0, len(state.SeenMessages))
		for id, message := range state.SeenMessages {
			items = append(items, item{id: id, time: message.Seen})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].time.Before(items[j].time) })
		for _, stale := range items[:len(items)-rosterMaxSeenIDs] {
			delete(state.SeenMessages, stale.id)
		}
	}
}

func storedRosterTextBytes(chunks map[int]rosterStoredChunk) int {
	total := 0
	for _, chunk := range chunks {
		if value, err := parseRosterPiece(chunk.Body); err == nil {
			total += len(value.text)
		}
	}
	return total
}

func cloneRosterTransferState(state rosterTransferDiskState) (rosterTransferDiskState, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return rosterTransferDiskState{}, err
	}
	var clone rosterTransferDiskState
	if err := json.Unmarshal(data, &clone); err != nil {
		return rosterTransferDiskState{}, err
	}
	ensureRosterMaps(&clone)
	return clone, nil
}

func ensureRosterMaps(state *rosterTransferDiskState) {
	if state.Groups == nil {
		state.Groups = map[string]*rosterTransferGroup{}
	}
	if state.Tombstones == nil {
		state.Tombstones = map[string]time.Time{}
	}
	if state.SeenMessages == nil {
		state.SeenMessages = map[string]rosterSeenMessage{}
	}
	for _, group := range state.Groups {
		if group != nil && group.Chunks == nil {
			group.Chunks = map[int]rosterStoredChunk{}
		}
	}
}

func (s *rosterTransferStore) loadLocked() error {
	if s.loaded {
		return s.loadErr
	}
	s.loaded = true
	if s.path == "" {
		s.loadErr = errors.New("roster state path is empty")
		return s.loadErr
	}
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.state = emptyRosterTransferState()
		return nil
	}
	if err != nil {
		s.loadErr = err
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		s.loadErr = errors.New("roster state path must be a regular file")
		return s.loadErr
	}
	if info.Size() > rosterMaxStateBytes {
		s.loadErr = errors.New("roster state exceeds the size limit")
		return s.loadErr
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		s.loadErr = err
		return err
	}
	var state rosterTransferDiskState
	if err := json.Unmarshal(data, &state); err != nil {
		s.loadErr = fmt.Errorf("parse roster state: %w", err)
		return s.loadErr
	}
	if _, err := decodeUniqueJSON(data); err != nil {
		s.loadErr = fmt.Errorf("parse roster state: %w", err)
		return s.loadErr
	}
	if state.FormatVersion != rosterStateFormat {
		s.loadErr = fmt.Errorf("unsupported roster state format %d", state.FormatVersion)
		return s.loadErr
	}
	ensureRosterMaps(&state)
	if err := validateRosterDiskState(state); err != nil {
		s.loadErr = err
		return err
	}
	s.state = state
	return nil
}

func validateRosterDiskState(state rosterTransferDiskState) error {
	if len(state.Groups) > rosterMaxActiveGroups || len(state.SeenMessages) > rosterMaxSeenIDs || len(state.Tombstones) > rosterMaxTombstones {
		return errors.New("roster state exceeds a collection limit")
	}
	if state.HighWater != nil && (state.HighWater.Version < 1 || !rosterHashPattern.MatchString(state.HighWater.Hash) ||
		(state.HighWater.State != "collecting" && state.HighWater.State != "ready" &&
			state.HighWater.State != "delivered" && state.HighWater.State != "expired" && state.HighWater.State != "rejected")) {
		return errors.New("roster state high-water mark is invalid")
	}
	for key, group := range state.Groups {
		if group == nil || group.Version < 1 || !rosterHashPattern.MatchString(group.Hash) ||
			(group.Status != "collecting" && group.Status != "ready") || len(group.Chunks) > rosterChunkLimit ||
			group.FirstSeen.IsZero() || group.Total < 0 || group.Total > rosterChunkLimit {
			return errors.New("roster state contains an invalid transfer group")
		}
		sender, version, hash, ok := parseRosterTransferKey(key)
		if !ok || version != group.Version || hash != group.Hash {
			return errors.New("roster state contains a transfer key mismatch")
		}
		if group.Total == 0 && (len(group.Chunks) > 0 || group.ManifestBody != "") {
			return errors.New("roster state contains pieces without a chunk count")
		}
		if group.ManifestBody == "" && group.ManifestMessageID != "" {
			return errors.New("roster state contains a manifest ID without a manifest")
		}
		if group.ManifestBody != "" {
			manifest, err := parseRosterPiece(group.ManifestBody)
			if err != nil || manifest.kind != rosterManifestType || manifest.issuer != sender ||
				manifest.version != group.Version || manifest.hash != group.Hash || manifest.total != group.Total ||
				group.ManifestMessageID == "" || len(group.ManifestMessageID) > 256 {
				return errors.New("roster state contains an invalid manifest")
			}
		}
		for index, stored := range group.Chunks {
			chunk, err := parseRosterPiece(stored.Body)
			if err != nil || chunk.kind != rosterChunkType || chunk.issuer != sender ||
				chunk.version != group.Version || chunk.hash != group.Hash || chunk.total != group.Total ||
				chunk.index != index || stored.MessageID == "" || len(stored.MessageID) > 256 {
				return errors.New("roster state contains an invalid chunk")
			}
		}
		if storedRosterTextBytes(group.Chunks) > rosterPayloadLimit {
			return errors.New("roster state chunks exceed the payload limit")
		}
		if group.Status == "ready" {
			if group.Payload == "" || len(group.Payload) > rosterPayloadLimit || group.FinalMessageID == "" ||
				len(group.FinalMessageID) > 256 || group.ManifestBody == "" || len(group.Chunks) != group.Total {
				return errors.New("roster state contains an invalid ready transfer")
			}
			payload, err := reassembleRosterTransfer(sender, sender, group.ManifestBody, group.Chunks)
			if err != nil || string(payload) != group.Payload {
				return errors.New("roster state ready payload failed revalidation")
			}
		} else if group.Payload != "" || group.FinalMessageID != "" {
			return errors.New("roster state collecting transfer has ready fields")
		}
	}
	for id, message := range state.SeenMessages {
		if id == "" || len(id) > 256 || message.Seen.IsZero() {
			return errors.New("roster state contains an invalid message ID record")
		}
		if _, _, _, ok := parseRosterTransferKey(message.Key); !ok {
			return errors.New("roster state contains an invalid message transfer key")
		}
	}
	for key, expiry := range state.Tombstones {
		if _, _, _, ok := parseRosterTransferKey(key); !ok || expiry.IsZero() {
			return errors.New("roster state contains an invalid tombstone")
		}
	}
	return nil
}

func (s *rosterTransferStore) commitLocked(next rosterTransferDiskState) error {
	ensureRosterMaps(&next)
	pruneRosterTransferState(&next, s.now().UTC())
	if err := validateRosterDiskState(next); err != nil {
		return err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(data) > rosterMaxStateBytes {
		return errors.New("roster state exceeds the size limit")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".a2a-roster-transfer-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	committed = true
	s.state = next
	dirFile, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := dirFile.Sync(); err != nil {
		_ = dirFile.Close()
		return err
	}
	if err := dirFile.Close(); err != nil {
		return err
	}
	return nil
}

func logRosterResult(message Inbound, result rosterReceiveResult) {
	if result.state == rosterReceiveRejected {
		slog.Warn("a2a: roster transfer rejected", "message_id", message.MessageID,
			"sender", message.SenderID, "reason", result.reason)
	}
}
