package a2a

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newRosterReceiverPlatform(t *testing.T, statePath string, now func() time.Time) (*Platform, *fakeAcker) {
	t.Helper()
	acker := newFakeAcker()
	p := newTestPlatform(t, &fakeClient{}, acker, map[string]any{
		"allow_from":    "saka,dot",
		"roster_issuer": "saka",
	})
	p.roster.store = newRosterTransferStore(statePath, now)
	return p, acker
}

func makeTestRoster(t *testing.T, issuer string, version int64, note string) []byte {
	t.Helper()
	roster := map[string]any{
		"type":        rosterLegacyRosterType,
		"version":     version,
		"issued_by":   issuer,
		"approved_by": "user",
		"members": []any{
			map[string]any{"id": "saka", "name": "saka", "full_name": "saka-claude-gocc"},
			map[string]any{"id": "peer", "name": "peer", "full_name": "peer-claude-codex"},
		},
		"note": note,
	}
	raw, err := json.Marshal(roster)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func updateRosterApprovalMetadata(t *testing.T, raw []byte, approvedBy, authorityRef string, hasAuthorityRef bool) []byte {
	t.Helper()
	var roster map[string]any
	if err := json.Unmarshal(raw, &roster); err != nil {
		t.Fatal(err)
	}
	roster["approved_by"] = approvedBy
	if hasAuthorityRef {
		roster["authority_ref"] = authorityRef
	} else {
		delete(roster, "authority_ref")
	}
	updated, err := json.Marshal(roster)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func makeRosterTransferBodies(t *testing.T, raw []byte, issuer string, version int64, partBytes int) (string, []string) {
	t.Helper()
	var source map[string]any
	if err := json.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	var chunks []string
	for offset := 0; offset < len(raw); offset += partBytes {
		end := offset + partBytes
		if end > len(raw) {
			end = len(raw)
		}
		chunks = append(chunks, string(raw[offset:end]))
	}
	manifestValue := map[string]any{
		"type":           rosterManifestType,
		"version":        version,
		"issued_by":      issuer,
		"approved_by":    source["approved_by"],
		"payload_format": rosterPayloadFormat,
		"source_sha256":  digest,
		"payload_sha256": digest,
		"payload_bytes":  len(raw),
		"chunks":         len(chunks),
	}
	if authorityRef, exists := source["authority_ref"]; exists {
		manifestValue["authority_ref"] = authorityRef
	}
	manifest, err := json.Marshal(manifestValue)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for index, text := range chunks {
		body, err := json.Marshal(map[string]any{
			"type":           rosterChunkType,
			"version":        version,
			"issued_by":      issuer,
			"source_sha256":  digest,
			"payload_sha256": digest,
			"index":          index,
			"total":          len(chunks),
			"text":           text,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > rosterBodyLimit {
			t.Fatalf("test chunk body is %d bytes", len(body))
		}
		bodies = append(bodies, string(body))
	}
	if len(manifest) > rosterBodyLimit {
		t.Fatalf("test manifest body is %d bytes", len(manifest))
	}
	return string(manifest), bodies
}

func rosterStoredChunks(chunks []string) map[int]rosterStoredChunk {
	stored := make(map[int]rosterStoredChunk, len(chunks))
	for index, body := range chunks {
		stored[index] = rosterStoredChunk{Body: body, MessageID: fmt.Sprintf("chunk-%d", index)}
	}
	return stored
}

func makeReadyRosterStore(t *testing.T, path string, now func() time.Time) (*rosterTransferStore, rosterPiece, string, string) {
	t.Helper()
	raw := makeTestRoster(t, "saka", 31, "ready replay")
	manifest, chunks := makeRosterTransferBodies(t, raw, "saka", 31, 1800)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want one", len(chunks))
	}
	manifestPiece, err := parseRosterPiece(manifest)
	if err != nil {
		t.Fatal(err)
	}
	finalPiece, err := parseRosterPiece(chunks[0])
	if err != nil {
		t.Fatal(err)
	}
	store := newRosterTransferStore(path, now)
	if result := store.add("manifest", "saka", "saka", manifestPiece); result.state != rosterReceivePending {
		t.Fatalf("manifest result = %+v, want pending", result)
	}
	if result := store.add("final", "saka", "saka", finalPiece); result.state != rosterReceiveReady {
		t.Fatalf("final chunk result = %+v, want ready", result)
	}
	return store, finalPiece, string(raw), rosterTransferKey("saka", 31, finalPiece.hash)
}

func TestRosterTransferReassemblesOutOfOrderAcrossRestartAndDeduplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster-state.json")
	raw := makeTestRoster(t, "saka", 31, strings.Repeat("x", 9000))
	manifest, chunks := makeRosterTransferBodies(t, raw, "saka", 31, 1800)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want a chunked fixture", len(chunks))
	}

	p1, _ := newRosterReceiverPlatform(t, path, time.Now)
	firstHandler := &recorder{}
	for i := len(chunks) - 1; i >= 1; i-- {
		p1.dispatch(context.Background(), firstHandler.handle, in(fmt.Sprintf("chunk-%d", i), "c1", "saka", chunks[i]))
	}
	if got := len(firstHandler.all()); got != 0 {
		t.Fatalf("incomplete chunks reached model: %d", got)
	}

	// A new receiver process sees the persisted out-of-order group.
	p2, _ := newRosterReceiverPlatform(t, path, time.Now)
	secondHandler := &recorder{}
	p2.dispatch(context.Background(), secondHandler.handle, in("manifest", "c1", "saka", manifest))
	if got := len(secondHandler.all()); got != 0 {
		t.Fatalf("manifest with a missing chunk reached model: %d", got)
	}
	final := in("chunk-0", "c1", "saka", chunks[0])
	p2.dispatch(context.Background(), secondHandler.handle, final)
	messages := secondHandler.all()
	if len(messages) != 1 || messages[0].Content != string(raw) || messages[0].UserID != "saka" {
		t.Fatalf("reconstructed message mismatch: count=%d message=%+v", len(messages), messages)
	}
	if messages[0].MessageID != final.MessageID {
		t.Fatalf("final message ID = %q, want %q", messages[0].MessageID, final.MessageID)
	}
	rc, ok := messages[0].ReplyCtx.(*replyContext)
	if !ok || rc.rosterTransferKey == "" || len(rc.MessageIDs) != 1 || rc.MessageIDs[0] != final.MessageID {
		t.Fatalf("unexpected reconstructed reply context: %#v", messages[0].ReplyCtx)
	}
	p2.FinishTurn(context.Background(), rc, true)

	// A process restart and transport replay cannot create a second model input.
	p3, replayAcker := newRosterReceiverPlatform(t, path, time.Now)
	replayHandler := &recorder{}
	p3.dispatch(context.Background(), replayHandler.handle, final)
	if got := len(replayHandler.all()); got != 0 {
		t.Fatalf("completed roster replay reached model: %d", got)
	}
	if got := replayAcker.ok; len(got) != 1 || got[0] != final.MessageID {
		t.Fatalf("replay was not transport-settled: %v", got)
	}
}

func TestRosterReadyReplaySurvivesSeenIDTTLEvictionAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster-state.json")
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	_, finalPiece, wantPayload, key := makeReadyRosterStore(t, path, clock)

	now = now.Add(rosterSeenIDTTL + time.Second)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pruned rosterTransferDiskState
	if err := json.Unmarshal(data, &pruned); err != nil {
		t.Fatal(err)
	}
	pruneRosterTransferState(&pruned, now)
	if _, exists := pruned.SeenMessages["final"]; exists {
		t.Fatal("final message ID survived the seen-ID TTL")
	}
	if group := pruned.Groups[key]; group == nil || group.Status != "ready" {
		t.Fatalf("ready group did not survive seen-ID TTL: %#v", group)
	}

	// A fresh platform models process restart; the final message ID is no longer in SeenMessages.
	restarted, acker := newRosterReceiverPlatform(t, path, clock)
	handler := &recorder{}
	restarted.dispatch(context.Background(), handler.handle, in("final", "c1", "saka", finalPiece.body))
	messages := handler.all()
	if len(messages) != 1 || messages[0].Content != wantPayload {
		t.Fatalf("final replay after seen-ID TTL reached handler as %d messages: %#v", len(messages), messages)
	}
	if len(acker.ok) != 0 {
		t.Fatalf("ready replay was acknowledged before its turn finished: %v", acker.ok)
	}
}

func TestRosterReadyReplaySurvivesSeenIDCapacityEvictionAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster-state.json")
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	store, finalPiece, wantPayload, key := makeReadyRosterStore(t, path, clock)

	store.mu.Lock()
	if err := store.loadLocked(); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	next, err := cloneRosterTransferState(store.state)
	if err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	next.SeenMessages["final"] = rosterSeenMessage{Key: key, Seen: now.Add(-time.Second)}
	for i := 0; i < rosterMaxSeenIDs-1; i++ {
		id := fmt.Sprintf("noise-%04d", i)
		next.SeenMessages[id] = rosterSeenMessage{Key: key, Seen: now.Add(time.Duration(i+1) * time.Nanosecond)}
	}
	if err := store.commitLocked(next); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	_, finalStillSeen := store.state.SeenMessages["final"]
	group := store.state.Groups[key]
	readyGroupSurvives := group != nil && group.Status == "ready"
	store.mu.Unlock()
	if finalStillSeen {
		t.Fatal("final message ID survived seen-ID capacity pruning")
	}
	if !readyGroupSurvives {
		t.Fatalf("ready group did not survive seen-ID capacity pruning: %#v", group)
	}

	// A fresh platform models process restart after capacity pruning.
	restarted, acker := newRosterReceiverPlatform(t, path, clock)
	handler := &recorder{}
	restarted.dispatch(context.Background(), handler.handle, in("final", "c1", "saka", finalPiece.body))
	messages := handler.all()
	if len(messages) != 1 || messages[0].Content != wantPayload {
		t.Fatalf("final replay after seen-ID capacity pruning reached handler as %d messages: %#v", len(messages), messages)
	}
	if len(acker.ok) != 0 {
		t.Fatalf("ready replay was acknowledged before its turn finished: %v", acker.ok)
	}
}

func TestRosterTransferKeepsOrdinaryAndLegacyMessagesCompatible(t *testing.T) {
	p, _ := newRosterReceiverPlatform(t, filepath.Join(t.TempDir(), "state.json"), time.Now)
	r := &recorder{}
	legacy := `{"type":"xstation.roster.v1","version":2,"issued_by":"saka","approved_by":"user"}`
	p.dispatch(context.Background(), r.handle, in("legacy", "c1", "saka", legacy))
	p.dispatch(context.Background(), r.handle, in("plain", "c1", "saka", "hello"))
	got := r.all()
	if len(got) != 2 || got[0].Content != legacy || got[1].Content != "hello" {
		t.Fatalf("legacy/ordinary messages changed path: %#v", got)
	}
}

func TestRosterApprovalMetadataAllowsLegacyAndFixedGrokOnly(t *testing.T) {
	tests := []struct {
		name            string
		approvedBy      string
		authorityRef    string
		hasAuthorityRef bool
		wantValid       bool
	}{
		{name: "legacy user", approvedBy: "user", wantValid: true},
		{name: "fixed grok", approvedBy: "grok", authorityRef: grokRosterAuthorityRef, hasAuthorityRef: true, wantValid: true},
		{name: "unknown approver", approvedBy: "saka", authorityRef: grokRosterAuthorityRef, hasAuthorityRef: true},
		{name: "grok missing reference", approvedBy: "grok"},
		{name: "grok wrong reference", approvedBy: "grok", authorityRef: "nibiru#1301", hasAuthorityRef: true},
		{name: "user with reference", approvedBy: "user", authorityRef: grokRosterAuthorityRef, hasAuthorityRef: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := makeTestRoster(t, "saka", 14, "approval metadata")
			raw = updateRosterApprovalMetadata(t, raw, tc.approvedBy, tc.authorityRef, tc.hasAuthorityRef)
			value, err := decodeUniqueJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			roster := value.(map[string]any)
			validationErr := validateRoster(roster, "saka")
			if (validationErr == nil) != tc.wantValid {
				t.Fatalf("validateRoster error = %v, want valid=%t", validationErr, tc.wantValid)
			}

			manifest, _ := makeRosterTransferBodies(t, raw, "saka", 14, 1800)
			_, manifestErr := parseRosterPiece(manifest)
			if (manifestErr == nil) != tc.wantValid {
				t.Fatalf("parseRosterPiece error = %v, want valid=%t", manifestErr, tc.wantValid)
			}
		})
	}
}

func TestRosterTransferDutyFallbackMatchesPythonFixture(t *testing.T) {
	fixtureBytes, err := os.ReadFile("testdata/roster_duty_fallback.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := decodeUniqueJSON(fixtureBytes)
	if err != nil {
		t.Fatal(err)
	}
	fixture, ok := value.(map[string]any)
	if !ok {
		t.Fatal("fixture must be an object")
	}
	cases, ok := fixture["cases"].([]any)
	if !ok || len(cases) == 0 {
		t.Fatal("fixture cases must be a non-empty array")
	}
	for _, value := range cases {
		caseValue, ok := value.(map[string]any)
		if !ok {
			t.Fatal("fixture case must be an object")
		}
		name, _ := caseValue["name"].(string)
		t.Run(name, func(t *testing.T) {
			roster, ok := caseValue["roster"].(map[string]any)
			if !ok {
				t.Fatal("case roster must be an object")
			}
			expectedIssuer, _ := caseValue["expected_issuer"].(string)
			wantValid, ok := caseValue["python_valid"].(bool)
			if expectedIssuer == "" || !ok {
				t.Fatal("case must include expected_issuer and python_valid")
			}
			raw, err := json.Marshal(roster)
			if err != nil {
				t.Fatal(err)
			}
			version, err := positiveJSONInteger(roster["version"], "version")
			if err != nil {
				t.Fatal(err)
			}
			manifest, chunks := makeRosterTransferBodies(t, raw, expectedIssuer, version, 1800)
			_, err = reassembleRosterTransfer(expectedIssuer, expectedIssuer, manifest, rosterStoredChunks(chunks))
			if (err == nil) != wantValid {
				t.Fatalf("reassembleRosterTransfer error = %v, want valid=%t", err, wantValid)
			}
		})
	}
}

func TestRosterTransferReassemblesFixedGrokApprovalMetadata(t *testing.T) {
	raw := makeTestRoster(t, "saka", 15, "grok approval")
	raw = updateRosterApprovalMetadata(t, raw, "grok", grokRosterAuthorityRef, true)
	manifest, chunks := makeRosterTransferBodies(t, raw, "saka", 15, 1800)

	reassembled, err := reassembleRosterTransfer("saka", "saka", manifest, rosterStoredChunks(chunks))
	if err != nil {
		t.Fatalf("reassemble fixed grok roster: %v", err)
	}
	if string(reassembled) != string(raw) {
		t.Fatalf("reassembled roster differs from source: %s", reassembled)
	}
}

func TestRosterTransferRejectsApprovalMetadataMismatchBetweenManifestAndSource(t *testing.T) {
	raw := makeTestRoster(t, "saka", 16, "grok approval")
	raw = updateRosterApprovalMetadata(t, raw, "grok", grokRosterAuthorityRef, true)
	manifest, chunks := makeRosterTransferBodies(t, raw, "saka", 16, 1800)

	var envelope map[string]any
	if err := json.Unmarshal([]byte(manifest), &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["approved_by"] = "user"
	delete(envelope, "authority_ref")
	manifestBytes, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseRosterPiece(string(manifestBytes)); err != nil {
		t.Fatalf("legacy user manifest should be independently valid: %v", err)
	}
	if _, err := reassembleRosterTransfer("saka", "saka", string(manifestBytes), rosterStoredChunks(chunks)); err == nil {
		t.Fatal("accepted manifest approval metadata that differs from source roster")
	}
}

func TestRosterIssuerMustAlreadyBeAllowlisted(t *testing.T) {
	if _, err := New(map[string]any{"allow_from": "dot", "roster_issuer": "saka"}); err == nil {
		t.Fatal("accepted roster_issuer absent from allow_from")
	}
}

func TestRosterTransferRejectsSenderMismatchAndDoesNotMixTransfers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	firstRaw := makeTestRoster(t, "saka", 7, strings.Repeat("a", 7000))
	secondRaw := makeTestRoster(t, "saka", 7, strings.Repeat("b", 7000))
	firstManifest, firstChunks := makeRosterTransferBodies(t, firstRaw, "saka", 7, 1500)
	secondManifest, secondChunks := makeRosterTransferBodies(t, secondRaw, "saka", 7, 1500)
	p, _ := newRosterReceiverPlatform(t, path, time.Now)
	r := &recorder{}

	// Authenticated sender mismatch is rejected before a group is created.
	p.dispatch(context.Background(), r.handle, in("wrong-sender", "c1", "dot", firstManifest))

	// Same issuer/version but a different digest remains a separate transfer.
	p.dispatch(context.Background(), r.handle, in("first-manifest", "c1", "saka", firstManifest))
	for i := 1; i < len(firstChunks); i++ {
		p.dispatch(context.Background(), r.handle, in(fmt.Sprintf("first-%d", i), "c1", "saka", firstChunks[i]))
	}
	for i := 1; i < len(firstChunks); i++ {
		p.dispatch(context.Background(), r.handle, in(fmt.Sprintf("cross-sender-%d", i), "c1", "dot", firstChunks[i]))
	}
	for i, body := range secondChunks {
		p.dispatch(context.Background(), r.handle, in(fmt.Sprintf("second-%d", i), "c1", "saka", body))
	}
	if got := len(r.all()); got != 0 {
		t.Fatalf("partial or mixed transfer reached model: %d", got)
	}
	final := in("first-0", "c1", "saka", firstChunks[0])
	p.dispatch(context.Background(), r.handle, final)
	got := r.all()
	if len(got) != 1 || got[0].Content != string(firstRaw) {
		t.Fatalf("first transfer did not reassemble alone: count=%d", len(got))
	}
	if secondManifest == firstManifest {
		t.Fatal("fixtures unexpectedly produced identical manifests")
	}
}

func TestRosterTransferDuplicateIndexRejectsWholeGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	raw := makeTestRoster(t, "saka", 9, strings.Repeat("x", 7000))
	manifest, chunks := makeRosterTransferBodies(t, raw, "saka", 9, 1600)
	p, _ := newRosterReceiverPlatform(t, path, time.Now)
	r := &recorder{}
	p.dispatch(context.Background(), r.handle, in("manifest", "c1", "saka", manifest))
	p.dispatch(context.Background(), r.handle, in("chunk-0", "c1", "saka", chunks[0]))
	p.dispatch(context.Background(), r.handle, in("chunk-0-again", "c1", "saka", chunks[0]))
	for i := 1; i < len(chunks); i++ {
		p.dispatch(context.Background(), r.handle, in(fmt.Sprintf("chunk-%d", i), "c1", "saka", chunks[i]))
	}
	if len(r.all()) != 0 {
		t.Fatalf("duplicate index transfer reached model: %d", len(r.all()))
	}
}

func TestRosterTransferExpiresAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	raw := makeTestRoster(t, "saka", 10, strings.Repeat("x", 7000))
	manifest, chunks := makeRosterTransferBodies(t, raw, "saka", 10, 1600)
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	p1, _ := newRosterReceiverPlatform(t, path, clock)
	r1 := &recorder{}
	p1.dispatch(context.Background(), r1.handle, in("manifest", "c1", "saka", manifest))
	p1.dispatch(context.Background(), r1.handle, in("chunk-0", "c1", "saka", chunks[0]))
	if len(r1.all()) != 0 {
		t.Fatalf("incomplete group reached model: %d", len(r1.all()))
	}

	// The partial manifest group and expiry state survive process replacement.
	now = now.Add(rosterTransferTTL + time.Second)
	p2, _ := newRosterReceiverPlatform(t, path, clock)
	r2 := &recorder{}
	p2.dispatch(context.Background(), r2.handle, in("chunk-1-after-expiry", "c1", "saka", chunks[1]))
	if len(r2.all()) != 0 {
		t.Fatalf("expired transfer reached model: %d", len(r2.all()))
	}
	p3, _ := newRosterReceiverPlatform(t, path, clock)
	r3 := &recorder{}
	p3.dispatch(context.Background(), r3.handle, in("chunk-2-after-restart", "c1", "saka", chunks[2]))
	if len(r3.all()) != 0 {
		t.Fatalf("persisted expired transfer reached model: %d", len(r3.all()))
	}
}

func TestRosterTransferRejectsLooseNumbersAndOversizedEnvelopes(t *testing.T) {
	digest := strings.Repeat("a", 64)
	badTotal := fmt.Sprintf(`{"type":%q,"version":1,"issued_by":"saka","source_sha256":%q,"payload_sha256":%q,"index":0,"total":1.0,"text":"x"}`, rosterChunkType, digest, digest)
	if _, err := parseRosterPiece(badTotal); err == nil {
		t.Fatal("accepted floating point chunk total")
	}
	badBool := strings.Replace(badTotal, `1.0`, `true`, 1)
	if _, err := parseRosterPiece(badBool); err == nil {
		t.Fatal("accepted boolean chunk total")
	}
	if _, err := decodeUniqueJSON([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("accepted duplicate JSON field")
	}
	if _, err := parseRosterPiece(string([]byte{'{', '"', 0xff, '"', ':', '1', '}'})); err == nil {
		t.Fatal("accepted an envelope with invalid UTF-8")
	}
	tooLargeManifest := fmt.Sprintf(`{"type":%q,"version":1,"issued_by":"saka","approved_by":"user","payload_format":%q,"source_sha256":%q,"payload_sha256":%q,"payload_bytes":65537,"chunks":1}`, rosterManifestType, rosterPayloadFormat, digest, digest)
	if _, err := parseRosterPiece(tooLargeManifest); err == nil {
		t.Fatal("accepted source larger than 65,536 bytes")
	}
	tooManyChunks := fmt.Sprintf(`{"type":%q,"version":1,"issued_by":"saka","approved_by":"user","payload_format":%q,"source_sha256":%q,"payload_sha256":%q,"payload_bytes":1,"chunks":33}`, rosterManifestType, rosterPayloadFormat, digest, digest)
	if _, err := parseRosterPiece(tooManyChunks); err == nil {
		t.Fatal("accepted more than 32 chunks")
	}
	oversized := `{"note":"` + strings.Repeat("x", rosterBodyLimit) + `","type":"` + rosterManifestType + `"}`
	if typ, ok := sniffRosterType(oversized); !ok || typ != rosterManifestType {
		t.Fatal("oversized transfer marker was not intercepted")
	}
	if _, err := parseRosterPiece(oversized); err == nil {
		t.Fatal("accepted oversized transfer body")
	}
	deep := strings.Repeat("[", rosterMaxJSONDepth+2) + "0" + strings.Repeat("]", rosterMaxJSONDepth+2)
	if _, err := decodeUniqueJSON([]byte(deep)); err == nil {
		t.Fatal("accepted excessively nested JSON")
	}
	ordinary := `{"note":"` + strings.Repeat("mentions "+rosterManifestType+" ", 400) + `"}`
	if _, ok := sniffRosterType(ordinary); ok {
		t.Fatal("ordinary JSON mentioning a transfer type was intercepted")
	}
}

func TestRosterTransferRejectsOversizedEscapedLateTypeBeforeModel(t *testing.T) {
	for _, typ := range []string{rosterManifestType, rosterChunkType} {
		name := strings.TrimPrefix(typ, "xstation.roster.")
		t.Run(name, func(t *testing.T) {
			body := `{"note":"` + strings.Repeat("x", rosterBodyLimit) + `","type":"\u0078` + strings.TrimPrefix(typ, "x") + `"}`
			if len(body) <= rosterBodyLimit {
				t.Fatalf("test body is %d bytes, want larger than %d", len(body), rosterBodyLimit)
			}
			if offset := strings.Index(body, `"type"`); offset <= rosterBodyLimit {
				t.Fatalf("type field starts at %d, want after %d-byte limit", offset, rosterBodyLimit)
			}

			p, _ := newRosterReceiverPlatform(t, filepath.Join(t.TempDir(), "roster-state.json"), time.Now)
			handler := &recorder{}
			p.dispatch(context.Background(), handler.handle, in("oversized-"+name, "c1", "saka", body))
			if got := handler.all(); len(got) != 0 {
				t.Fatalf("oversized escaped roster transfer reached model: got %d messages", len(got))
			}
		})
	}
}

func TestRosterTransferRejectsTamperedHashAndInvalidRosterBeforeHandler(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func([]string) []string
		payload []byte
	}{
		{
			name:    "hash mismatch",
			payload: makeTestRoster(t, "saka", 12, strings.Repeat("x", 5000)),
			mutate: func(chunks []string) []string {
				var envelope map[string]any
				if err := json.Unmarshal([]byte(chunks[0]), &envelope); err != nil {
					t.Fatal(err)
				}
				text := envelope["text"].(string)
				envelope["text"] = "z" + text[1:]
				body, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				chunks[0] = string(body)
				return chunks
			},
		},
		{
			name:    "schema validation",
			payload: []byte(`{"type":"xstation.roster.v1","version":12,"issued_by":"saka","approved_by":"user"}`),
			mutate:  func(chunks []string) []string { return chunks },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			manifest, chunks := makeRosterTransferBodies(t, tc.payload, "saka", 12, 1500)
			chunks = tc.mutate(chunks)
			p, _ := newRosterReceiverPlatform(t, path, time.Now)
			r := &recorder{}
			p.dispatch(context.Background(), r.handle, in("manifest", "c1", "saka", manifest))
			for i, body := range chunks {
				p.dispatch(context.Background(), r.handle, in(fmt.Sprintf("chunk-%d", i), "c1", "saka", body))
			}
			if len(r.all()) != 0 {
				t.Fatalf("unvalidated roster reached handler: %d", len(r.all()))
			}
		})
	}
}

func TestRosterTransferStorageFailureLeavesMessageRetryable(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "regular-file")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(blocker, "state.json")
	p, acker := newRosterReceiverPlatform(t, statePath, time.Now)
	p.roster.store.loaded = true
	p.roster.store.state = emptyRosterTransferState()
	raw := makeTestRoster(t, "saka", 13, strings.Repeat("x", 5000))
	manifest, _ := makeRosterTransferBodies(t, raw, "saka", 13, 1800)
	r := &recorder{}
	p.dispatch(context.Background(), r.handle, in("manifest", "c1", "saka", manifest))
	if len(r.all()) != 0 {
		t.Fatalf("storage failure reached handler: %d", len(r.all()))
	}
	p.mu.Lock()
	_, completed := p.done["manifest"]
	p.mu.Unlock()
	if len(acker.failed) != 0 || len(acker.ok) != 0 || completed {
		t.Fatalf("storage failure was not left retryable: failed=%v acked=%v", acker.failed, acker.ok)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blocker, 0700); err != nil {
		t.Fatal(err)
	}
	p.dispatch(context.Background(), r.handle, in("manifest", "c1", "saka", manifest))
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("retry did not persist the message: %v", err)
	}
	if len(acker.ok) != 1 || acker.ok[0] != "manifest" {
		t.Fatalf("persisted retry was not transport-acked: %v", acker.ok)
	}
}
