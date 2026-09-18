import { useCallback, useEffect, useMemo, useState } from "react"
import { ZodError } from "zod"

import {
  ApiError,
  createRoom as createRoomRequest,
  createDM as createDMRequest,
  getDM,
  getRoom,
  listAgents,
  listDMs,
  listRooms,
  observeRoom,
  observeDM,
  postMessage,
  postDM,
  runActionJob,
  runAction,
  usingMock,
} from "@/lib/api"
import { ALL_SEATS } from "@/lib/contracts"
import type {
  AgentOption,
  DMEvent,
  DMSummary,
  LiveEvent,
  MeetEvent,
  RoomDetail,
  RoomSummary,
  State,
} from "@/lib/contracts"

export type ConnectionStatus = "connecting" | "open" | "closed"

/* THE SEND IS PLAIN, AND THAT IS A DECISION.
 *
 * A hold-then-dispatch machine with a cancel and a server-side recall lived
 * here and did not work. It is removed rather than repaired: story
 * e9f5d325eded records what came out, and a later sprint owns rebuilding it.
 *
 * The reasoning is worth keeping because whoever rebuilds it will need it.
 * Aborting an in-flight request cannot mean "not sent": it cancels the
 * RESPONSE, never the effect, and both synchronous paths — a plain post and a
 * 1:1 — write their record inside the request, so a post-dispatch cancel is a
 * race the sender loses. Only a hold before dispatch can promise a message
 * never left the browser. That is what made the feature expensive: it needed
 * its own state machine, an expiry timer, a superseding rule for the next
 * message, and a confirm on the delivered branch but not the held one.
 *
 * What replaces it is what the composer actually needs: send, an accepted
 * indicator, and the response when it arrives.
 */

type ConversationKind = "room" | "dm"

function linkedConversation(): { kind: ConversationKind; ref: string } | null {
  const params = new URLSearchParams(window.location.search)
  const dm = params.get("dm")?.trim()
  if (dm) return { kind: "dm", ref: dm }
  const room = params.get("room")?.trim()
  if (room) return { kind: "room", ref: room }
  return null
}

function rememberConversation(kind: ConversationKind, ref: string) {
  const next = new URL(window.location.href)
  next.searchParams.delete("room")
  next.searchParams.delete("dm")
  next.searchParams.set(kind, ref)
  window.history.replaceState(null, "", next)
}

export function useMeetRoom() {
  const [draft] = useState(
    () => new URLSearchParams(window.location.search).get("draft") ?? "",
  )
  const [rooms, setRooms] = useState<RoomSummary[]>([])
  const [dms, setDMs] = useState<DMSummary[]>([])
  const [agents, setAgents] = useState<AgentOption[]>([])
  const [selectedRef, setSelectedRef] = useState("")
  const [selectedKind, setSelectedKind] = useState<"room" | "dm">("room")
  const [viewKind, setViewKind] = useState<"room" | "dm">("room")
  const [detail, setDetail] = useState<RoomDetail | null>(null)
  const [state, setState] = useState<State | null>(null)
  const [events, setEvents] = useState<MeetEvent[]>([])
  const [live, setLive] = useState<LiveEvent | null>(null)
  const [connection, setConnection] =
    useState<ConnectionStatus>("connecting")
  const [observeRevision, setObserveRevision] = useState(0)
  // The raw-transport view, off by default. It lives here rather than in the
  // message list because the server decides what to send: turning it on
  // reconnects the socket with ?raw=1, so an ordinary session never carries the
  // extra bytes.
  const [debugRaw, setDebugRaw] = useState(false)
  const [queued, setQueued] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [sending, setSending] = useState(false)
  const [creating, setCreating] = useState(false)

  useEffect(() => {
    let active = true
    const linked = linkedConversation()
    const openChat =
      new URLSearchParams(window.location.search).get("chat") === "1"
    // Opening a 1:1 link is the same operation as choosing an agent from the
    // Chat picker: ensure its durable DM exists, then load the ordinary lists.
    // A sprint-room link is read-only and needs no corresponding mutation.
    const prepare = linked?.kind === "dm" && !usingMock
      ? createDMRequest(linked.ref)
      : Promise.resolve()
    prepare
      .then(() => Promise.all([listRooms(), listAgents(), listDMs()]))
      .then(([nextRooms, nextAgents, nextDMs]) => {
        if (!active) return
        setRooms(nextRooms)
        setAgents(nextAgents)
        setDMs(nextDMs)
        if (linked) {
          setSelectedKind(linked.kind)
          setViewKind(linked.kind)
          setSelectedRef(linked.ref)
        } else if (openChat) {
          setSelectedKind("dm")
          setViewKind("dm")
          setSelectedRef("")
        } else {
          setSelectedRef((current) => current || nextRooms[0]?.id || "")
        }
      })
      .catch((reason: unknown) => {
        if (active) setError(messageFor(reason))
      })
    return () => {
      active = false
    }
  }, [])

  const selectRoom = useCallback((ref: string) => {
    setViewKind("room")
    setSelectedKind("room")
    setSelectedRef(ref)
    rememberConversation("room", ref)
  }, [])

  const selectDM = useCallback((agent: string) => {
    setViewKind("dm")
    setSelectedKind("dm")
    setSelectedRef(agent)
    rememberConversation("dm", agent)
  }, [])

  useEffect(() => {
    if (!selectedRef) return
    let active = true
    setEvents([])
    setLive(null)
    setError(null)
    if (selectedKind === "dm") {
      // Fetch and observe the SAME projection. If the HTTP snapshot omitted
      // raw while the socket included it, their identical event keys made the
      // winner timing-dependent when the reader toggled the debug view.
      getDM(selectedRef, debugRaw)
        .then((nextDetail) => {
          if (!active) return
          setDetail(null)
          setState(dmState(nextDetail.state))
          setEvents(nextDetail.events.map(dmMeetEvent))
        })
        .catch((reason: unknown) => {
          if (active) setError(messageFor(reason))
        })
      const stop = observeDM(
        selectedRef,
        (event) => {
          if (active) {
            setEvents((current) => addUnique(current, dmMeetEvent(event)))
            if (event.role === "assistant") setLive(null)
          }
        },
        (progress) => {
          if (!active) return
          if (progress.kind === "spoke") {
            setLive(null)
            return
          }
          setLive((current) => ({
            ...progress,
            text:
              progress.text && current?.speaker === progress.speaker && current.text
                ? `${current.text}\n${progress.text}`
                : progress.text || current?.text || "",
          }))
        },
        setConnection,
        debugRaw,
      )
      return () => {
        active = false
        stop()
      }
    }
    getRoom(selectedRef)
      .then((nextDetail) => {
        if (!active) return
        setDetail(nextDetail)
        setState(nextDetail.state)
      })
      .catch((reason: unknown) => {
        if (active) setError(messageFor(reason))
      })

    const stop = observeRoom(
      selectedRef,
      (frame) => {
        if (!active) return
        if (frame.kind === "info") setState(frame.data)
        if (frame.kind === "event") {
          setEvents((current) => addUnique(current, frame.data))
        }
        if (frame.kind === "live") {
          if (frame.data.kind === "spoke") {
            setLive(null)
          } else if (frame.data.kind === "speaking") {
            setLive(frame.data)
          } else {
            setLive((current) => ({
              ...frame.data,
              text:
                current?.speaker === frame.data.speaker &&
                current.round === frame.data.round &&
                current.text
                  ? `${current.text}\n${frame.data.text}`
                  : frame.data.text,
            }))
          }
        }
      },
      setConnection,
      debugRaw,
    )
    return () => {
      active = false
      stop()
    }
  }, [selectedRef, selectedKind, observeRevision, debugRaw])

  // send posts the message and reports what the host said about it. There is
  // no hold and no pending record: the click sends.
  const send = useCallback(
    async (text: string, agent?: string) => {
      if (!selectedRef || !state) return
      setSending(true)
      setError(null)
      setQueued(null)
      try {
        if (selectedKind === "dm") {
          setLive({
            kind: "speaking",
            round: 0,
            speaker: selectedRef,
            role: "assistant",
            text: "",
            status: "working",
            lines: 0,
            bytes: 0,
            elapsed_ms: 0,
          })
          await postDM(selectedRef, text)
          setQueued(`Your message to ${selectedRef} was accepted; the reply will appear here.`)
        } else if (agent === ALL_SEATS) {
          // A broadcast is MAIL, not a floor: it lands in every participant's
          // inbox and waits to be read. Running N turns from one browser
          // message is what `meet round` is for, and it is chaired.
          const event = await postMessage(selectedRef, state.human, text, ALL_SEATS)
          setEvents((current) => addUnique(current, event))
          setQueued("Delivered to everyone in the room. Each participant sees it as their own mail.")
        } else if (agent) {
          await runActionJob(selectedRef, "address", { agent, text })
          setQueued(`Your message to ${agent} was accepted; the reply will appear here.`)
        } else {
          const event = await postMessage(selectedRef, state.human, text)
          setEvents((current) => addUnique(current, event))
        }
      } catch (reason) {
        if (selectedKind === "dm") setLive(null)
        if (reason instanceof ApiError && reason.status === 409) {
          setQueued(
            agent
              ? `Your message to ${agent} is queued for the next turn.`
              : "Your request is queued for the next turn.",
          )
        } else {
          setError(messageFor(reason))
        }
      } finally {
        setSending(false)
      }
    },
    [selectedRef, selectedKind, state],
  )

  const act = useCallback(
    async (action: string, label: string, body?: unknown) => {
      if (!selectedRef) return false
      setSending(true)
      setError(null)
      setQueued(null)
      try {
        await runAction(selectedRef, action, body)
        // The roster lives on State, and State reaches the browser exactly once
        // — in the info frame sent when /observe connects. A verb that changes
        // membership therefore leaves the member list stale until a reload, so
        // re-read the room after one. The turn verbs need no such thing: their
        // output arrives as transcript events, which the socket does stream.
        if (["invite", "kick", "mark", "open", "close"].includes(action)) {
          const refreshed = await getRoom(selectedRef)
          setDetail(refreshed)
          setState(refreshed.state)
          if (action === "open") {
            setEvents([])
            setLive(null)
            setObserveRevision((current) => current + 1)
          }
          setRooms(await listRooms())
        }
        return true
      } catch (reason) {
        if (reason instanceof ApiError && reason.status === 409) {
          setQueued(`${label} is queued while the current turn finishes.`)
        } else if (reason instanceof ApiError && reason.status === 403) {
          setError("This control is available to the room organizer.")
        } else {
          setError(messageFor(reason))
        }
        return false
      } finally {
        setSending(false)
      }
    },
    [selectedRef],
  )

  /** createRoom opens a room and switches to it. The room list is re-fetched
   * rather than patched: the server assigns the room number and the seat the
   * caller took, and guessing either would put a number on screen that a reload
   * corrects. Returns whether it worked, so the dialog knows to close. */
  const createRoom = useCallback(
    async (topic: string, owner: string, participants: string[]) => {
      setCreating(true)
      setError(null)
      try {
        const created = await createRoomRequest({ topic, owner, participants })
        const nextRooms = await listRooms()
        setRooms(nextRooms)
        selectRoom(created.id)
        return true
      } catch (reason) {
        setError(messageFor(reason))
        return false
      } finally {
        setCreating(false)
      }
    },
    [selectRoom],
  )

  const createDM = useCallback(async (agent: string) => {
    setCreating(true)
    setError(null)
    try {
      await createDMRequest(agent)
      setDMs(await listDMs())
      selectDM(agent)
      return true
    } catch (reason) {
      setError(messageFor(reason))
      return false
    } finally {
      setCreating(false)
    }
  }, [selectDM])

  const isOrganizer = useMemo(
    () => Boolean(state && state.initiator === state.human),
    [state],
  )

  // Who a message goes to when the reader has not typed "@name".
  //
  // A room's DEFAULT recipient is its owner — the project manager in a
  // sprint's room. That is not a convenience: unaddressed mail in such a room
  // already lands on that seat server-side, so a composer that sent to nobody
  // was disagreeing with the room it was typing into. `""` is a real, choosable
  // value meaning the whole room, which is why this is a string and not a
  // nullable name.
  //
  // The choice is per ROOM and per BROWSER, so it is remembered here rather
  // than on the room: two people in one room may each be talking to a different
  // agent, and writing one reader's pick onto shared state would move the other
  // reader's recipient under them.
  const [recipientByRoom, setRecipientByRoom] = useState<Record<string, string>>(
    () => readStoredRecipients(),
  )
  // The owner when there is one, a real broadcast when there is not. Never the
  // empty string: that posts room history addressed to nobody, which lands in
  // no inbox and wakes no one — indistinguishable, from the outside, from every
  // agent in the room ignoring you.
  const owner = state?.owner || ALL_SEATS
  const recipient =
    selectedKind === "room" && selectedRef
      ? (recipientByRoom[selectedRef] ?? owner)
      : ""
  const setRecipient = useCallback(
    (next: string) => {
      if (!selectedRef) return
      setRecipientByRoom((current) => {
        const updated = { ...current, [selectedRef]: next }
        writeStoredRecipients(updated)
        return updated
      })
    },
    [selectedRef],
  )

  return {
    agents,
    rooms,
    dms,
    selectedRef,
    selectedKind,
    viewKind,
    selectMode: setViewKind,
    selectRoom,
    selectDM,
    detail,
    state,
    events,
    live,
    connection,
    queued,
    dismissQueued: () => setQueued(null),
    error,
    sending,
    send,
    act,
    createRoom,
    createDM,
    creating,
    isOrganizer,
    recipient,
    setRecipient,
    usingMock,
    debugRaw,
    setDebugRaw,
    draft,
  }
}

function dmState(dm: DMSummary): State {
  return {
    id: `dm:${dm.agent}`,
    board: false,
    room: dm.agent,
    name: dm.agent,
    topic: "Direct message",
    agenda: [],
    participants: [
      { name: dm.human, role: "human", live: true },
      { name: dm.agent, role: "agent", live: true },
    ],
    secretary: "",
    chair: "",
    human: dm.human,
    status: "open",
    cwd: "",
    out: "",
    round: 0,
    initiator: dm.human,
    decision_mode: "",
    // A direct message has one recipient and it is the agent named in it.
    // Saying so keeps the composer's recipient logic uniform instead of
    // special-casing the DM view into a second code path.
    owner: dm.agent,
    owner_title: "agent",
    // A chat has no seat to be late-bound to: the recipient IS the agent, and
    // the room-side label ("conductor:99") has no counterpart here.
    default_to: "",
  }
}

function dmMeetEvent(event: DMEvent): MeetEvent {
  return {
    // The record's OWN kind wins where it has one. A chat projects everything
    // onto user/assistant, and under that projection a retraction would arrive
    // as an ordinary message from the human while the message it withdraws
    // still read as live — the one thing this feature must not do, in the
    // surface a person is most likely to be reading.
    kind:
      event.kind === "retraction"
        ? "retraction"
        : event.role === "human"
          ? "human"
          : "turn",
    speaker: event.speaker,
    role: event.role,
    // A 1:1 has one counterpart, so the addressee is implied and the message
    // list does not print one. Carrying an invented name here would put the
    // same "to" under every message and say nothing.
    to: "",
    text: event.text,
    ts: event.ts,
    status: event.status,
    retracts: event.retracts,
    raw: event.raw,
  }
}

function eventKey(event: MeetEvent) {
  return [event.kind, event.speaker, event.ts, event.text].join("|")
}

function addUnique(current: MeetEvent[], event: MeetEvent) {
  const key = eventKey(event)
  return current.some((item) => eventKey(item) === key)
    ? current
    : [...current, event]
}

function messageFor(reason: unknown) {
  if (reason instanceof ZodError) {
    return "Meet received an incompatible response. Refresh the page; if it persists, restart `bashy meet serve`."
  }
  if (reason instanceof Error) return reason.message
  return "Something went wrong. Please try again."
}

// The recipient picks live in localStorage, keyed by room.
//
// Wrapped in try/catch on BOTH sides: a private window, cleared site data, or
// a browser configured to block storage makes the accessor itself throw, and a
// composer that cannot remember a preference must still send messages.
const RECIPIENT_STORE = "bashy.meet.recipient"

function readStoredRecipients(): Record<string, string> {
  try {
    const raw = window.localStorage.getItem(RECIPIENT_STORE)
    if (!raw) return {}
    const parsed: unknown = JSON.parse(raw)
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return {}
    const out: Record<string, string> = {}
    for (const [room, name] of Object.entries(parsed)) {
      if (typeof name === "string") out[room] = name
    }
    return out
  } catch {
    return {}
  }
}

function writeStoredRecipients(value: Record<string, string>) {
  try {
    window.localStorage.setItem(RECIPIENT_STORE, JSON.stringify(value))
  } catch {
    /* a reader who cannot store a preference still gets this session's. */
  }
}
