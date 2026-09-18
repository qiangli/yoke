# Canonical agent claim keys

## Goal

Make every live singleton surface address a registered agent through one room
claim key. Fleet names remain the public identity; the room key preserves the
existing chat-safe spelling used by session storage and control paths.

## Contract

1. `room.AgentClaimID` is the only fleet-name to singleton-card-key mapping.
2. Chat session creation, authored-actor validation, and `whois` all use it.
3. Readers fall back to an already-live raw-name card so an in-flight watcher
   from the pre-contract implementation remains visible and authorized until it
   exits. New cards always use the canonical key.
4. The public agent name remains in `Card.Nick`; only the room storage/claim key
   is normalized.

## Verification

Cover the exact existing chat normalization, dotted registered names, authored
session acceptance/refusal, `whois` `TAKEN` projection, and legacy raw-card
fallback. Run the focused room, bus, principal, and chat suites plus crossvet.
