import { z } from "zod"

export const memberSchema = z.union([
  z.string(),
  z.object({
    name: z.string(),
    role: z.string().optional(),
    live: z.boolean().default(false),
  }),
])

export const agentOptionSchema = z.object({
  name: z.string(),
  nick: z.string().optional(),
  binding: z.string().optional(),
  band: z.number().optional(),
  ephemeral: z.boolean().optional(),
  task: z.string().optional(),
  available: z.boolean(),
  reason: z.string().optional(),
})

export const roomSummarySchema = z.object({
  id: z.string(),
  // The server sends the room NUMBER as an int (RoomSummary.Room is a Go int),
  // so a bare z.string() rejects every real response and the list silently
  // renders empty — which is what it did until a browser test looked. The mock
  // fixtures used strings, so the mismatch was invisible in dev.
  room: z.union([z.string(), z.number()]).optional(),
  name: z.string().optional(),
  permanent: z.boolean().optional(),
  // A board is a room whose floor is never run for it: the server refuses
  // round/poll/ask/converge there (409 wrong-mode), so the UI reads this to
  // not offer them. omitempty: absent means an ordinary meeting.
  board: z.boolean().default(false),
  topic: z.string(),
  status: z.string(),
  members: z.array(memberSchema),
  updated: z.union([z.string(), z.number()]),
})

export const eventKindSchema = z.enum([
  "agenda",
  "human",
  "turn",
  "vote",
  "poll",
  "question",
  "ledger",
  "replan",
  "note",
  "decision",
  "action",
  "confirm",
  "invite",
  "kick",
  // A withdrawal. It is a RECORD, not an edit: the transcript is append-only
  // and the message it withdraws stays where it was, so a reader (and an agent
  // that already read the original) sees the correction rather than a hole.
  "retraction",
])

export const eventSchema = z
  .object({
    round: z.number().optional(),
    speaker: z.string().default("System"),
    role: z.string().default("system"),
    kind: eventKindSchema,
    // Who the message is FOR. Marshalled omitempty, and an absent key is a real
    // state rather than a missing field: it means shared room history, which is
    // what every agent turn is (a reply names nobody, so it wakes nobody). A
    // name or a seat label ("conductor:99") means directed mail; ALL_SEATS
    // means an explicit broadcast.
    to: z.string().default(""),
    text: z.string().default(""),
    file: z.string().optional(),
    ts: z.union([z.string(), z.number()]).optional(),
    status: z.string().optional(),
    exit_code: z.number().optional(),
    chars: z.number().optional(),
    duration_ms: z.number().optional(),
    question: z.string().optional(),
    choice: z.string().optional(),
    choices: z.array(z.string()).optional(),
    ledger: z.unknown().optional(),
    // On a `retraction`: the `ts` of the record it withdraws. Events carry no
    // id, so the timestamp is the handle — the same one this file's eventKey
    // already dedupes on.
    retracts: z.string().optional(),
    // The agent's un-normalized output, sent only when the reader turned the
    // raw-transport view on. Absent on every ordinary frame.
    raw: z.string().optional(),
  })
  .passthrough()

// LiveEvent (live.go) marks role/text/status `omitempty`, so a `speaking` frame
// carries none of them — only `spoke` sets status, and role is empty for a
// plain agent turn. Requiring them made the SPA drop EVERY live frame as
// invalid: the room painted its history and then never moved, so an addressed
// agent's reply was received and silently discarded.
//
// kind/round/speaker/ts have no omitempty and are always sent, so they stay
// required — that is what makes this a contract rather than a shrug.
export const liveEventSchema = z.object({
  kind: z.enum(["speaking", "line", "spoke"]),
  round: z.number(),
  speaker: z.string(),
  role: z.string().default(""),
  text: z.string().default(""),
  status: z.string().default(""),
  ts: z.union([z.string(), z.number()]).optional(),
  ctl_sock: z.string().optional(),
  // Bounded Chat progress counters. Ordinary Meet live frames omit them.
  lines: z.number().optional(),
  bytes: z.number().optional(),
  elapsed_ms: z.number().optional(),
})

// The server's State marshals most fields with `omitempty`, so a room that has
// no chair, no agenda and no rounds yet simply does not send those keys — and a
// zero round is a MISSING key, not 0. Requiring them rejected every real
// response, which is why the room header sat on "Opening room…" forever while
// the mock fixtures (which spell every field) looked fine.
//
// Rule for this file: model what the SERVER sends, not what the mock does. Give
// every omitempty field a default, and accept both spellings where Go's type and
// the mock's disagree (room is an int; turn_timeout marshals as "20m").
export const stateSchema = z
  .object({
    schema: z.union([z.string(), z.number()]).optional(),
    id: z.string(),
    room: z.union([z.string(), z.number()]).optional(),
    name: z.string().optional(),
    permanent: z.boolean().optional(),
    // Same contract as roomSummarySchema.board: marshalled omitempty, so a
    // missing key IS an ordinary meeting and only `true` marks a board.
    board: z.boolean().default(false),
    role_holders: z.record(z.string(), z.string()).optional(),
    topic: z.string().default(""),
    agenda: z.array(z.string()).default([]),
    participants: z.array(memberSchema).default([]),
    secretary: z.string().default(""),
    secretary_pending: z.boolean().optional(),
    secretary_band: z.number().optional(),
    chair: z.string().default(""),
    human: z.string().default(""),
    status: z.string().default("open"),
    cwd: z.string().default(""),
    out: z.string().default(""),
    turn_timeout: z.union([z.string(), z.number()]).optional(),
    created: z.union([z.string(), z.number()]).optional(),
    round: z.number().default(0),
    initiator: z.string().default(""),
    decision_mode: z.string().default(""),
    // Who is accountable for this room, resolved by the SERVER at read time.
    //
    // The room stores a late-bound seat label ("conductor:99") so a handover
    // re-targets mail already in flight; only the host holds the table that
    // maps that seat to whoever sits in it today. So the browser is told the
    // answer rather than deriving one — and owner is empty when the seat is
    // VACANT, which is a real state and not a missing field.
    owner: z.string().default(""),
    owner_title: z.string().default(""),
    // The room's late-bound default addressee, held as a LABEL
    // ("conductor:99") and never as a holder. It is what an unaddressed post
    // becomes server-side, so the message list needs it to recognise the
    // resolved owner behind a seat address instead of printing the raw label.
    default_to: z.string().default(""),
  })
  .passthrough()

export const synthesisSchema = z
  .object({
    agenda: z.array(z.string()).optional(),
    decisions: z.array(z.string()).optional(),
    minutes: z.array(z.string()).optional(),
  })
  .passthrough()

export const roomDetailSchema = z.object({
  state: stateSchema,
  synthesis: synthesisSchema.nullable(),
})

export const dmSummarySchema = z.object({
  agent: z.string(),
  human: z.string().default("you"),
  created: z.union([z.string(), z.number()]),
  updated: z.union([z.string(), z.number()]),
})

export const dmEventSchema = z.object({
  id: z.string(),
  speaker: z.string(),
  role: z.string(),
  text: z.string(),
  ts: z.union([z.string(), z.number()]),
  status: z.string().optional(),
  // The underlying record's own kind and, on a retraction, the `ts` it
  // withdraws. Both optional: an older server projects a chat onto
  // user/assistant alone, and a client that required them would refuse every
  // frame from it.
  kind: z.string().optional(),
  retracts: z.string().optional(),
  raw: z.string().optional(),
})

export const dmDetailSchema = z.object({
  state: dmSummarySchema,
  // Empty Go slices should marshal as [], but older Relay builds emitted null
  // before the first message. Both mean the same thing: no transcript yet.
  // Normalizing at the wire boundary keeps transport representation out of the
  // UI and prevents a Zod diagnostic blob from becoming the conversation.
  events: z.array(dmEventSchema).nullish().transform((events) => events ?? []),
})

export const dmObserveFrameSchema = z.discriminatedUnion("kind", [
  z.object({ kind: z.literal("dm-event"), data: dmEventSchema }),
  z.object({ kind: z.literal("dm-progress"), data: liveEventSchema }),
])

export const jobRefSchema = z.object({
  id: z.string().optional(),
  job: z.string().optional(),
  ref: z.string().optional(),
})

// The 202 a chat's send answers with. `ts` names the record it just wrote, and
// is optional because an older server does not send one — a client without it
// simply has no handle to recall by, which is a missing capability rather than
// a broken response.
export const dmSendSchema = z.object({
  agent: z.string().optional(),
  status: z.string().optional(),
  ts: z.string().optional(),
})

// What a recall ACHIEVED, decided by the server and never by the client.
//
// The three values are exclusive and a client renders one sentence for each. It
// must not collapse "gone" into "canceled": the first means the system no longer
// knows, the second promises the message never went out.
export const recallSchema = z.object({
  verdict: z.enum(["canceled", "retracted", "gone"]),
  event: eventSchema.optional(),
  retracted: z.string().optional(),
})

export const errorSchema = z.object({ error: z.string() })

export const observeFrameSchema = z.discriminatedUnion("kind", [
  z.object({ kind: z.literal("info"), data: stateSchema }),
  z.object({ kind: z.literal("event"), data: eventSchema }),
  z.object({ kind: z.literal("history-end"), note: z.string() }),
  z.object({ kind: z.literal("live"), data: liveEventSchema }),
])

export type RecallResult = z.infer<typeof recallSchema>
export type Member = z.infer<typeof memberSchema>
export type AgentOption = z.infer<typeof agentOptionSchema>
export type RoomSummary = z.infer<typeof roomSummarySchema>
export type MeetEvent = z.infer<typeof eventSchema>
export type LiveEvent = z.infer<typeof liveEventSchema>
export type State = z.infer<typeof stateSchema>
export type Synthesis = z.infer<typeof synthesisSchema>
export type RoomDetail = z.infer<typeof roomDetailSchema>
export type ObserveFrame = z.infer<typeof observeFrameSchema>
export type DMSummary = z.infer<typeof dmSummarySchema>
export type DMEvent = z.infer<typeof dmEventSchema>
export type DMDetail = z.infer<typeof dmDetailSchema>

/** ALL_SEATS is meet's explicit broadcast addressee (meet.AllSeats).
 *
 * Sending to it is NOT the same as sending to nobody: an unaddressed post is
 * room history that no participant owes a reply to and that dispatch wakes
 * nobody for, which is why "Everyone" cannot simply be the empty string.
 */
export const ALL_SEATS = "all"

export function memberName(member: Member): string {
  return typeof member === "string" ? member : member.name
}

export function memberIsLive(member: Member): boolean {
  return typeof member === "string" ? false : member.live
}

export function memberRole(member: Member): string | undefined {
  return typeof member === "string" ? undefined : member.role
}
