import type {
  AgentOption,
  MeetEvent,
  ObserveFrame,
  RoomDetail,
  RoomSummary,
  State,
} from "./contracts"

export const mockAgents: AgentOption[] = [
  { name: "codex", nick: "Patch", binding: "codex:gpt5.6", band: 3, available: true },
  { name: "claude", nick: "Sage", binding: "claude:opus", band: 4, available: true },
  { name: "agy", nick: "Atlas", binding: "agy:opus", band: 3, available: false, reason: "tool unavailable" },
]

export async function mockListAgents(): Promise<AgentOption[]> {
  return structuredClone(mockAgents)
}

const now = Date.now()

export const mockRooms: RoomSummary[] = [
  {
    id: "launch-room",
    board: false,
    room: "Launch room",
    topic: "Plan the customer preview",
    status: "open",
    members: [
      { name: "Mira Chen", role: "human", live: true },
      { name: "Atlas", role: "research", live: true },
      { name: "Patch", role: "engineering", live: false },
      { name: "Sage", role: "strategy", live: true },
    ],
    updated: new Date(now - 90_000).toISOString(),
  },
  {
    id: "quality-circle",
    board: false,
    room: "Quality circle",
    topic: "Release readiness",
    status: "open",
    members: [
      { name: "Mira Chen", role: "human", live: true },
      { name: "Patch", role: "engineering", live: false },
    ],
    updated: new Date(now - 3_600_000).toISOString(),
  },
  {
    id: "research-notes",
    board: false,
    room: "Research notes",
    topic: "What customers need next",
    status: "closed",
    members: [
      { name: "Mira Chen", role: "human", live: false },
      { name: "Atlas", role: "research", live: false },
    ],
    updated: new Date(now - 86_400_000).toISOString(),
  },
]

export const mockState: State = {
  schema: "meet/v1",
  id: "launch-room",
  board: false,
  room: "Launch room",
  topic: "Plan the customer preview",
  agenda: [
    "Agree on the preview audience",
    "Choose the smallest convincing demo",
    "Name owners for launch follow-up",
  ],
  participants: mockRooms[0].members,
  secretary: "",
  chair: "",
  human: "Mira Chen",
  status: "open",
  cwd: "/workspace/launch",
  out: "minutes.md",
  turn_timeout: 120,
  created: new Date(now - 7_200_000).toISOString(),
  round: 3,
  initiator: "Alex Rivera",
  decision_mode: "consent",
  // The demo room has a facilitator, so the composer's recipient default has
  // something to point at. A mock that omitted it would show "Everyone" and
  // quietly demo a different product than the one that ships.
  owner: "Atlas",
  owner_title: "facilitator",
  // An ordinary meeting advertises no late-bound seat; a sprint's room does
  // ("conductor:99"). The demo is the ordinary case.
  default_to: "",
}

export const mockDetail: RoomDetail = {
  state: mockState,
  synthesis: {
    agenda: mockState.agenda,
    decisions: [
      "Preview will focus on the guided room workflow.",
      "Invite five design partners before a broader launch.",
    ],
    minutes: [
      "Atlas summarized nine interviews and highlighted setup friction.",
      "The room aligned on a 12-minute, outcome-led preview.",
      "Patch will prepare the stable demo environment.",
    ],
  },
}

export const mockEvents: MeetEvent[] = [
  {
    round: 1,
    speaker: "System",
    role: "system",
    kind: "agenda",
    to: "",
    text: "Agenda set: agree on the preview audience and choose the smallest convincing demo.",
    ts: new Date(now - 6_900_000).toISOString(),
  },
  {
    round: 1,
    speaker: "Mira Chen",
    role: "human",
    kind: "human",
    // An explicit broadcast — the demo shows the two addressed shapes a room
    // really has, because a fixture that only ever posts to nobody demos a
    // different product than the one that ships.
    to: "all",
    text: "We have one hour. I’d like us to leave with a preview story that feels useful, not theatrical.",
    ts: new Date(now - 6_600_000).toISOString(),
  },
  {
    round: 1,
    speaker: "Atlas",
    role: "research",
    kind: "turn",
    to: "",
    text: "Across **nine customer interviews**, the strongest signal was confidence during setup.\n\nI’d frame the preview around one promise:\n\n> Bring a complicated question into a room and leave with a clear, owned decision.\n\nThat gives us a human story while still showing multiple agents collaborating.",
    status: "complete",
    duration_ms: 8200,
    ts: new Date(now - 6_300_000).toISOString(),
  },
  {
    round: 2,
    speaker: "System",
    role: "system",
    kind: "invite",
    to: "",
    text: "Sage joined the room at Mira’s invitation.",
    ts: new Date(now - 5_700_000).toISOString(),
  },
  {
    round: 2,
    speaker: "Sage",
    role: "strategy",
    kind: "turn",
    to: "",
    text: "A concise flow could be:\n\n1. Name the outcome in everyday language.\n2. Let the room surface evidence and disagreement.\n3. Capture the decision and owner automatically.\n\nThe product should feel like a **calm facilitator**, not another dashboard.",
    status: "complete",
    duration_ms: 6100,
    ts: new Date(now - 5_100_000).toISOString(),
  },
  {
    round: 3,
    speaker: "System",
    role: "system",
    kind: "decision",
    to: "",
    text: "Decision recorded: preview the guided room workflow with five design partners.",
    ts: new Date(now - 3_900_000).toISOString(),
  },
  {
    round: 3,
    speaker: "System",
    role: "system",
    kind: "action",
    to: "",
    text: "Action: Patch owns the stable demo environment by Thursday.",
    ts: new Date(now - 3_600_000).toISOString(),
  },
  {
    round: 3,
    speaker: "Mira Chen",
    role: "human",
    kind: "human",
    to: "Atlas",
    text: "Atlas, can you turn that into a two-sentence opening for the preview?",
    ts: new Date(now - 210_000).toISOString(),
  },
]

type Listener = (frame: ObserveFrame) => void
const listeners = new Set<Listener>()
let events = [...mockEvents]
let busyOnce = true

export class MockHttpError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
  }
}

export async function mockListRooms(): Promise<RoomSummary[]> {
  await pause(120)
  return mockRooms
}

export async function mockGetRoom(ref: string): Promise<RoomDetail> {
  await pause(100)
  if (!mockRooms.some((room) => room.id === ref)) {
    throw new MockHttpError(404, "That room could not be found.")
  }
  return ref === mockState.id
    ? mockDetail
    : {
        state: {
          ...mockState,
          id: ref,
          room: mockRooms.find((room) => room.id === ref)?.room ?? ref,
          topic: mockRooms.find((room) => room.id === ref)?.topic ?? "",
        },
        synthesis: null,
      }
}

export async function mockCreateRoom(
  topic: string,
  participants: string[],
  owner: string,
): Promise<State> {
  await pause(140)
  if (!topic.trim()) {
    throw new MockHttpError(400, "A room needs a topic.")
  }
  if (participants.length === 0) {
    throw new MockHttpError(400, "Invite at least one agent.")
  }
  if (!owner.trim()) {
    throw new MockHttpError(400, "Choose a facilitator.")
  }
  const id = `demo-${mockRooms.length + 1}`
  const room = String(mockRooms.length + 1)
  mockRooms.unshift({
    id,
    board: false,
    room,
    topic,
    status: "open",
    members: participants,
    updated: new Date().toISOString(),
  })
  return { ...mockState, id, room, topic, participants, chair: owner, owner, owner_title: "facilitator", round: 0 }
}

export async function mockPost(
  _ref: string,
  author: string,
  text: string,
): Promise<MeetEvent> {
  await pause(180)
  const event: MeetEvent = {
    round: mockState.round,
    speaker: author,
    role: "human",
    kind: "human",
    to: "",
    text,
    ts: new Date().toISOString(),
  }
  events = [...events, event]
  emit({ kind: "event", data: event })
  return event
}

export async function mockAction(
  action: string,
  _body?: unknown,
): Promise<{ id: string } | undefined> {
  await pause(260)
  if (action === "invite" || action === "kick" || action === "close") {
    throw new MockHttpError(403, "Only the room organizer can do that.")
  }
  if (busyOnce && ["address", "round", "poll", "ask", "converge"].includes(action)) {
    busyOnce = false
    throw new MockHttpError(409, "The room is finishing another turn.")
  }
  if (action === "mark") return undefined
  return { id: `mock-${Date.now()}` }
}

export function observeMock(listener: Listener): () => void {
  listeners.add(listener)
  const timers = [
    window.setTimeout(() => listener({ kind: "info", data: mockState }), 80),
    ...events.map((event, index) =>
      window.setTimeout(
        () => listener({ kind: "event", data: event }),
        110 + index * 18,
      ),
    ),
    window.setTimeout(
      () =>
        listener({
          kind: "history-end",
          note: `${events.length} event(s) of history`,
        }),
      140 + events.length * 18,
    ),
    window.setTimeout(
      () =>
        listener({
          kind: "live",
          data: {
            kind: "speaking",
            round: 4,
            speaker: "Atlas",
            role: "research",
            text: "",
            status: "running",
            ts: new Date().toISOString(),
            ctl_sock: "mock",
          },
        }),
      650,
    ),
    window.setTimeout(
      () =>
        listener({
          kind: "live",
          data: {
            kind: "line",
            round: 4,
            speaker: "Atlas",
            role: "research",
            text: "Teams move faster when the conversation, evidence, and decision stay together.",
            status: "running",
            ts: new Date().toISOString(),
            ctl_sock: "mock",
          },
        }),
      1250,
    ),
  ]

  return () => {
    listeners.delete(listener)
    timers.forEach(window.clearTimeout)
  }
}

function emit(frame: ObserveFrame) {
  listeners.forEach((listener) => listener(frame))
}

function pause(ms: number) {
  return new Promise((resolve) => window.setTimeout(resolve, ms))
}
