import {
  agentOptionSchema,
  dmDetailSchema,
  dmEventSchema,
  dmObserveFrameSchema,
  dmSendSchema,
  dmSummarySchema,
  errorSchema,
  eventSchema,
  jobRefSchema,
  observeFrameSchema,
  recallSchema,
  roomDetailSchema,
  roomSummarySchema,
  stateSchema,
  type MeetEvent,
  type AgentOption,
  type ObserveFrame,
  type RoomDetail,
  type RoomSummary,
  type State,
  type DMDetail,
  type DMEvent,
  type DMSummary,
  type LiveEvent,
  type RecallResult,
} from "./contracts"
import {
  MockHttpError,
  mockAction,
  mockListAgents,
  mockCreateRoom,
  mockGetRoom,
  mockListRooms,
  mockPost,
  observeMock,
} from "./mock"

const params = new URLSearchParams(window.location.search)
export const usingMock =
  params.get("mock") === "1" ||
  (import.meta.env.DEV && params.get("mock") !== "0")

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message)
  }
}

export async function listDMs(): Promise<DMSummary[]> {
  if (usingMock) return []
  return dmSummarySchema.array().parse(await request("api/dms"))
}

export async function createDM(agent: string): Promise<DMSummary> {
  return dmSummarySchema.parse(await request("api/dms", {
    method: "POST",
    body: JSON.stringify({ agent }),
  }))
}

export async function getDM(agent: string, debugRaw = false): Promise<DMDetail> {
  const url = `api/dms/${encodeURIComponent(agent)}${debugRaw ? "?raw=1" : ""}`
  return dmDetailSchema.parse(await request(url))
}

/** postDM sends into a 1:1 and returns the `ts` of the record it wrote.
 *
 * The timestamp is the handle a recall names. A chat appends the human's line
 * INSIDE this request — that is what makes the reply streamable — so there is
 * no job to cancel afterwards, and naming the record is the only way to ask for
 * it back.
 */
export async function postDM(agent: string, text: string): Promise<string> {
  const result = await request(`api/dms/${encodeURIComponent(agent)}/messages`, {
    method: "POST",
    body: JSON.stringify({ text }),
  })
  const parsed = dmSendSchema.safeParse(result)
  return parsed.success ? (parsed.data.ts ?? "") : ""
}

/** recallDM withdraws a chat message, by the ts postDM returned. */
export async function recallDM(agent: string, ts: string): Promise<RecallResult> {
  return recallSchema.parse(
    await request(`api/dms/${encodeURIComponent(agent)}/recall`, {
      method: "POST",
      body: JSON.stringify({ ts }),
    }),
  )
}

// api/dms/<agent>/work — the managed write-capable session — is deliberately
// NOT reachable from here. A 1:1 has one action, sending, and the browser had
// no way to answer a vendor CLI's approval prompt, so the control it offered
// was one the server refused outside proven containment. The endpoint remains
// for callers that can prove it (see pkg/meet/relay_dm.go).

export function observeDM(
  agent: string,
  onEvent: (event: DMEvent) => void,
  onProgress: (event: LiveEvent) => void,
  onStatus: (status: "connecting" | "open" | "closed") => void,
  debugRaw = false,
): () => void {
  const url = new URL("observe-dm", document.baseURI)
  url.protocol = window.location.protocol === "https:" ? "wss:" : "ws:"
  url.searchParams.set("agent", agent)
  if (debugRaw) url.searchParams.set("raw", "1")
  let stopped = false
  let socket: WebSocket | null = null
  let retry: number | null = null
  const connect = () => {
    if (stopped) return
    onStatus("connecting")
    const next = new WebSocket(url)
    socket = next
    next.addEventListener("open", () => onStatus("open"))
    next.addEventListener("close", () => {
      if (stopped) return
      onStatus("closed")
      retry = window.setTimeout(connect, 500)
    })
    next.addEventListener("error", () => next.close())
    next.addEventListener("message", (message) => {
      const result = dmObserveFrameSchema.safeParse(JSON.parse(String(message.data)))
      if (!result.success) return
      if (result.data.kind === "dm-event") {
        onEvent(dmEventSchema.parse(result.data.data))
      } else {
        onProgress(result.data.data)
      }
    })
  }
  connect()
  return () => {
    stopped = true
    if (retry !== null) window.clearTimeout(retry)
    socket?.close()
    onStatus("closed")
  }
}

function endpoint(path: string): URL {
  return new URL(path.replace(/^\/+/, ""), document.baseURI)
}

async function request(path: string, init?: RequestInit): Promise<unknown> {
  const response = await fetch(endpoint(path), {
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...init?.headers,
    },
  })
  if (!response.ok) {
    const body = errorSchema.safeParse(await response.json().catch(() => ({})))
    throw new ApiError(
      response.status,
      body.success ? body.data.error : `Request failed (${response.status})`,
    )
  }
  return response.status === 204 ? undefined : response.json()
}

function normalizeError(error: unknown): never {
  if (error instanceof MockHttpError) {
    throw new ApiError(error.status, error.message)
  }
  throw error
}

export async function listRooms(): Promise<RoomSummary[]> {
  if (usingMock) return roomSummarySchema.array().parse(await mockListRooms())
  return roomSummarySchema.array().parse(await request("api/rooms"))
}

export async function listAgents(): Promise<AgentOption[]> {
  if (usingMock) return agentOptionSchema.array().parse(await mockListAgents())
  return agentOptionSchema.array().parse(await request("api/agents"))
}

export async function getRoom(ref: string): Promise<RoomDetail> {
  if (usingMock) {
    try {
      return roomDetailSchema.parse(await mockGetRoom(ref))
    } catch (error) {
      normalizeError(error)
    }
  }
  return roomDetailSchema.parse(
    await request(`api/rooms/${encodeURIComponent(ref)}`),
  )
}

/** Browser meeting creation is intentionally narrow. The human must explicitly
 * choose a registered facilitator and at least one separate participant; the
 * server owns all lifecycle and execution controls. */
export interface NewRoom {
  topic: string
  participants: string[]
  owner: string
}

export async function createRoom(input: NewRoom): Promise<State> {
  if (usingMock) {
    try {
      return stateSchema.parse(await mockCreateRoom(input.topic, input.participants, input.owner))
    } catch (error) {
      normalizeError(error)
    }
  }
  return stateSchema.parse(
    await request("api/rooms", {
      method: "POST",
      body: JSON.stringify(input),
    }),
  )
}

/** postMessage appends a human contribution, optionally ADDRESSED.
 *
 * `to` is what decides whether anyone is accountable for it: ALL_SEATS puts it
 * in every participant's inbox, a name puts it in one, and omitting it posts
 * room history addressed to nobody. The route accepted no addressee at all
 * until now, so the browser could only ever produce the last of those.
 */
export async function postMessage(
  ref: string,
  author: string,
  text: string,
  to?: string,
): Promise<MeetEvent> {
  if (usingMock) return eventSchema.parse(await mockPost(ref, author, text))
  return eventSchema.parse(
    await request(`api/rooms/${encodeURIComponent(ref)}/post`, {
      method: "POST",
      body: JSON.stringify({ author, text, to: to ?? "" }),
    }),
  )
}

/** recall asks the server to stop a message, and reports what that achieved.
 *
 * Both handles are sent when the caller has both: the JOB is the only one that
 * can still produce a clean cancel, and the `ts` is what remains once the job
 * has finished. The verdict is the SERVER's — a browser cannot observe whether
 * the record was written, and a UI that guessed would be claiming a fact it
 * cannot see.
 */
export async function recall(
  ref: string,
  handle: { job?: string; ts?: string },
): Promise<RecallResult> {
  if (usingMock) return { verdict: "canceled" }
  return recallSchema.parse(
    await request(`api/rooms/${encodeURIComponent(ref)}/recall`, {
      method: "POST",
      body: JSON.stringify({ job: handle.job ?? "", ts: handle.ts ?? "" }),
    }),
  )
}

/** runActionJob is runAction for the verbs that answer with a JOB REF.
 *
 * It returns the job id so a caller can recall the dispatch it just started;
 * runAction stays as the void-returning form every other control uses.
 */
export async function runActionJob(
  ref: string,
  action: string,
  body?: unknown,
): Promise<string> {
  if (usingMock) {
    await runAction(ref, action, body)
    return ""
  }
  const result = await request(
    `api/rooms/${encodeURIComponent(ref)}/${action}`,
    { method: "POST", body: body === undefined ? undefined : JSON.stringify(body) },
  )
  const parsed = jobRefSchema.safeParse(result)
  return parsed.success ? (parsed.data.job ?? parsed.data.id ?? "") : ""
}

export async function runAction(
  ref: string,
  action: string,
  body?: unknown,
): Promise<void> {
  if (usingMock) {
    try {
      const result = await mockAction(action, body)
      if (result) jobRefSchema.parse(result)
      return
    } catch (error) {
      normalizeError(error)
    }
  }
  const result = await request(
    `api/rooms/${encodeURIComponent(ref)}/${action}`,
    {
      method: "POST",
      body: body === undefined ? undefined : JSON.stringify(body),
    },
  )
  if (result !== undefined) {
    if (action === "mark") eventSchema.parse(result)
    else if (action === "open") stateSchema.parse(result)
    else jobRefSchema.parse(result)
  }
}

export function observeRoom(
  ref: string,
  onFrame: (frame: ObserveFrame) => void,
  onStatus: (status: "connecting" | "open" | "closed") => void,
  debugRaw = false,
): () => void {
  if (usingMock) {
    onStatus("open")
    const stop = observeMock((frame) => onFrame(observeFrameSchema.parse(frame)))
    return () => {
      stop()
      onStatus("closed")
    }
  }

  const url = new URL("observe", document.baseURI)
  url.protocol = window.location.protocol === "https:" ? "wss:" : "ws:"
  url.searchParams.set("room", ref)
  // The raw view is a per-CONNECTION choice, not a per-message one: asking for
  // it on every socket would double the wire size of a transcript to serve a
  // debugging aid that is off by default. Toggling it reconnects.
  if (debugRaw) url.searchParams.set("raw", "1")

  let stopped = false
  let socket: WebSocket | null = null
  let retry: number | null = null
  let retryDelay = 250

  const connect = () => {
    if (stopped) return
    onStatus("connecting")
    const next = new WebSocket(url)
    socket = next
    next.addEventListener("open", () => {
      if (stopped || socket !== next) return
      retryDelay = 250
      onStatus("open")
    })
    next.addEventListener("close", (event) => {
      if (socket === next) socket = null
      if (stopped) return
      onStatus("closed")
      // A normal close is how the server says this room ended. Reopening the
      // room changes the hook's observe revision and creates a fresh socket.
      // Every other close (service restart, network loss, tunnel flap) retries
      // automatically, so a browser tab never needs a manual refresh merely
      // because its backend restarted.
      if (event.code === 1000) return
      retry = window.setTimeout(connect, retryDelay)
      retryDelay = Math.min(retryDelay * 2, 5_000)
    })
    next.addEventListener("error", () => next.close())
    next.addEventListener("message", (message) => {
      try {
        const result = observeFrameSchema.safeParse(
          JSON.parse(String(message.data)),
        )
        if (result.success) onFrame(result.data)
        else console.warn("Ignored invalid meet frame", result.error)
      } catch (error) {
        console.warn("Ignored unreadable meet frame", error)
      }
    })
  }

  connect()
  return () => {
    stopped = true
    if (retry !== null) window.clearTimeout(retry)
    socket?.close()
    socket = null
    onStatus("closed")
  }
}
