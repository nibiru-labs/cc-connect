# Nibiru #1318: cc-connect bounded roster receiver

## Scope

Adapt only `platform/a2a` to receive the bounded roster protocol from
`nibiru-labs/nibiru#1321` at `637602005cc4b10bda95ef711a8fcff32239f2f2`.
The manifest and chunk JSON shapes and their limits remain exactly as specified
there; this receiver does not define a parallel wire protocol.

The receiver intercepts only `xstation.roster.manifest.v1` and
`xstation.roster.chunk.v1`. It holds each message outside the normal message
handler until a complete roster has passed reassembly and validation. Ordinary
A2A messages and legacy `xstation.roster.v1` bodies retain the existing path.

## Trust boundary

- The authenticated sender is `Inbound.SenderID`, supplied by the A2A transport.
- `roster_issuer` is an optional local configuration value. When enabled, it
  must name an identity already present in local `allow_from`; it is the
  receiver's trusted `expected_issuer`. It is never inferred from manifest,
  chunk, or reconstructed roster data. If it is unset or not locally allowed,
  bounded roster messages are rejected and acknowledged without reaching the
  model.
- Transport sender, `expected_issuer`, manifest `issued_by`, every chunk's
  `issued_by`, and reconstructed roster `issued_by` must all match.
- `approved_by: "user"` is validated as roster data only. It is not approval
  evidence and does not authorize roster, ACL, credential, or runtime changes.

## Reassembly and persistence

- Preserve the wire limits: 4,096 UTF-8 bytes per body, 65,536 source bytes,
  and at most 32 chunks. Require strict integer fields, unique JSON object keys,
  UTF-8, matching version/count/hash/issuer/charter, full SHA-256 and byte
  length checks, then the roster schema checks before forwarding the original
  source JSON once to the existing handler.
- Accept out-of-order chunks, including chunks received before the manifest.
  Bind stored pieces to authenticated sender, expected issuer, version, and
  source hash. A repeated transport message ID is idempotent; a second manifest
  or second chunk for an occupied index under a different message ID rejects
  that transfer. A piece from another sender/version/hash cannot join it.
- Persist transfer state atomically in the user's cache directory before
  acknowledging an incomplete piece. Keep a one-hour first-seen TTL, at most
  eight active groups, and bounded message-ID/tombstone history. Completed
  issuer/version/hash state survives restarts so a replay cannot trigger a
  second model delivery. If durable state cannot be read or written, fail
  closed and leave the current message retryable.
- A complete group is delivered as one `core.Message`; intermediate pieces do
  not enter the model path. Intermediate transport messages may be ACKed after
  their bytes are durably stored. The final message remains on the existing
  turn ACK path. The response is only a transport receipt; no roster file,
  allowlist, ACL, or credential is written by the receiver.
- Expired or structurally invalid groups are tombstoned and discarded as a
  whole. The receiver never forwards a partial roster.

## Verification

Before receiver integration, the focused regression was RED:

```text
$ go test ./platform/a2a -run '^TestRosterTransferIncompletePartsDoNotReachModel$' -count=1
--- FAIL: TestRosterTransferIncompletePartsDoNotReachModel
    a2a_test.go:89: incomplete roster parts reached model: got 2 messages, want 0
```

Tests cover that old per-message path, ordinary/legacy compatibility,
persistent out-of-order assembly across receiver restarts, transport replay and
duplicate indexes, expiry, sender/version/hash isolation, resource limits, and
validation before handler delivery. No live A2A message, deployment, runtime
allowlist, or credential is changed.
