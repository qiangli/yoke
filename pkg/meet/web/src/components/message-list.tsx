import { useEffect, useMemo, useRef } from "react"
import {
  ArrowRight,
  Bot,
  CheckCircle2,
  CircleDotDashed,
  ClipboardCheck,
  ListTodo,
  Megaphone,
  UserRound,
  UserRoundPlus,
  Vote,
} from "lucide-react"
import ReactMarkdown from "react-markdown"
import rehypeSanitize from "rehype-sanitize"

import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { Badge } from "@/components/ui/badge"
import type { LiveEvent, MeetEvent, State } from "@/lib/contracts"
import { addresseeOf, isGenericTitle, seatOf, type Seat } from "@/lib/seats"
import { cn } from "@/lib/utils"

interface MessageListProps {
  events: MeetEvent[]
  live: LiveEvent | null
  /** The room, so a message can say who its speaker and addressee ARE here —
   * not merely that both are "participants". */
  state: State | null
  kind?: "room" | "dm"
  // debugRaw is the reader's JSON view: every message body is replaced by the
  // record it was rendered from.
  //
  // It used to mean ONLY "carry the un-normalized transport alongside", and
  // that made the control dead. The server sends `raw` exclusively when
  // normalization CHANGED the stored text — but every capture seam now
  // normalizes BEFORE writing (engine.go's turn recorder, relay_dm.go's DM
  // turn), so a record written by this build never differs from its render and
  // `raw` is never populated. The toggle therefore had a visible effect only on
  // transcripts left by an older build. A JSON view of the record itself is
  // always available, which is what the button says it does.
  debugRaw?: boolean
}

const systemKinds = new Set([
  // A retraction is ABOUT a message rather than another message. Rendering it
  // as a full bubble would put the withdrawn text on screen twice, in a larger
  // typeface than the thing it withdraws.
  "retraction",
  "agenda",
  "ledger",
  "replan",
  "note",
  "decision",
  "action",
  "confirm",
  "invite",
  "kick",
  "vote",
  "poll",
  "question",
])

export function MessageList({
  events,
  live,
  state,
  kind = "room",
  debugRaw = false,
}: MessageListProps) {
  const agent = kind === "dm" ? state?.name || state?.owner || "" : ""
  // Which records have been withdrawn, resolved at READ time from the
  // retractions in the transcript rather than stored on the message itself.
  // The transcript is append-only, so the message cannot be edited to say so —
  // and resolving here means a retraction that arrives later marks its target
  // the moment it lands, with no second write.
  const retracted = useMemo(() => {
    const marked = new Set<string>()
    for (const event of events) {
      if (event.kind === "retraction" && event.retracts) marked.add(event.retracts)
    }
    return marked
  }, [events])
  const endRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    endRef.current?.scrollIntoView({ behavior: "smooth", block: "end" })
  }, [events.length, live?.text])

  return (
    <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain px-4 py-6 sm:px-8">
      <div className="mx-auto w-full max-w-[780px]">
        <div className="mb-8 rounded-2xl border border-border/70 bg-card/60 px-5 py-4 shadow-sm shadow-slate-900/[0.025]">
          <div className="mb-1 flex items-center gap-2 text-[11px] font-bold uppercase tracking-[0.14em] text-primary">
            <Megaphone className="size-3.5" />
            {kind === "dm" ? "Direct message" : "Room opened"}
          </div>
          <p className="text-sm leading-relaxed text-muted-foreground">
            {kind === "dm" ? (
              <>
                A private, governed Chat conversation with{" "}
                {/* Named, not merely implied. Every message below carries the
                    same "@name", so the one thing a 1:1 leaves out — the
                    addressee — is the only thing this view has to state once. */}
                <span className="font-medium text-foreground">
                  {agent ? `@${agent}` : "one registered agent"}
                </span>
                .
              </>
            ) : (
              "This is a shared conversation. Messages, decisions, and actions stay together as the room works."
            )}
          </p>
        </div>

        <div className="space-y-1">
          {events.map((event, index) =>
            systemKinds.has(event.kind) ? (
              <SystemEvent
                debugRaw={debugRaw}
                event={event}
                key={eventKey(event, index)}
              />
            ) : (
              <Message
                debugRaw={debugRaw}
                event={event}
                key={eventKey(event, index)}
                kind={kind}
                retracted={retracted.has(stampOf(event))}
                state={state}
              />
            ),
          )}
          {live && <LiveMessage live={live} state={state} />}
        </div>
        <div ref={endRef} />
      </div>
    </div>
  )
}

function Message({
  event,
  state,
  kind,
  retracted = false,
  debugRaw = false,
}: {
  event: MeetEvent
  state: State | null
  kind: "room" | "dm"
  /** The sender withdrew this message. It STAYS — see the note on retractions
   * in the transcript: the record is append-only, and an agent that already
   * read the original needs to see that it was withdrawn, not find a hole. */
  retracted?: boolean
  debugRaw?: boolean
}) {
  const from = seatOf(event.speaker, state, event.role)
  const isHuman = from.kind === "human" || event.kind === "human"
  // WHO IT IS FOR, and only where that is not already known.
  //
  // A 1:1 has exactly one other party, so an addressee line there would repeat
  // the same name under every message. A room is many-to-many: "@codex said
  // this" does not say whether it was asked of the project manager, of one
  // participant, or of everybody, and that is the difference between a
  // transcript and a conversation you can follow.
  const to = kind === "room" ? addresseeOf(event, state) : null
  return (
    <article className="group flex gap-3 rounded-xl px-2 py-4 transition-colors hover:bg-white/55 sm:gap-4 sm:px-3">
      <Avatar className="mt-0.5 size-9 shrink-0 border border-border/70 shadow-sm">
        <AvatarFallback
          className={cn(
            "font-semibold",
            isHuman
              ? "bg-violet-100 text-violet-700"
              : "bg-teal-100 text-teal-800",
          )}
        >
          {isHuman ? (
            <UserRound className="size-4" />
          ) : (
            <Bot className="size-4" />
          )}
        </AvatarFallback>
      </Avatar>
      <div className="min-w-0 flex-1">
        {/* One stable hook for the whole attribution line — who spoke, who it
            was for, what they are called here. Asserting on the article alone
            cannot tell a badge that says "participant" from an agent that used
            the word in its answer. */}
        <div
          className="mb-1 flex flex-wrap items-baseline gap-x-2 gap-y-1"
          data-message-header
        >
          <SeatName seat={from} />
          {to && (
            <span className="flex items-baseline gap-1">
              {/* The arrow is decoration; the relationship is read aloud. */}
              <ArrowRight
                aria-hidden
                className="size-3 shrink-0 self-center text-muted-foreground/60"
              />
              <span className="sr-only">to</span>
              <SeatName muted seat={to} />
            </span>
          )}
          <RoleBadge seat={from} />
          <time className="text-[10px] text-muted-foreground/70">
            {formatTime(event.ts)}
          </time>
          {retracted && (
            <span className="rounded-full bg-amber-100 px-1.5 py-0.5 text-[9px] font-semibold uppercase tracking-wide text-amber-900">
              Retracted
            </span>
          )}
        </div>
        {debugRaw ? (
          <EventRecord event={event} />
        ) : isHuman ? (
          <p
            className={cn(
              "whitespace-pre-wrap text-[14px] leading-6 text-foreground/90",
              retracted && "text-muted-foreground/60 line-through",
            )}
          >
            {event.text}
          </p>
        ) : (
          <div className="markdown text-[14px] leading-6 text-foreground/90">
            <ReactMarkdown rehypePlugins={[rehypeSanitize]}>
              {event.text}
            </ReactMarkdown>
          </div>
        )}
        {debugRaw && event.raw && <RawTransport raw={event.raw} />}
      </div>
    </article>
  )
}

/** SeatName prints a name the way the composer ADDRESSES it — "@codex" — so
 * the transcript and the box you type into use one spelling. A person is
 * printed bare: meet routes seats, and a human is not one. */
function SeatName({ seat, muted = false }: { seat: Seat; muted?: boolean }) {
  return (
    <span
      className={cn(
        "truncate",
        muted
          ? "text-[12px] font-medium text-muted-foreground"
          : "text-[13px] font-semibold text-foreground",
      )}
      title={seat.label ? `seat ${seat.label} · ${seat.title}` : seat.title}
    >
      {seat.handle}
    </span>
  )
}

/** RoleBadge says what the speaker is CALLED in this room.
 *
 * Every agent turn is recorded with role "participant" — that is the meeting's
 * role, and it was on every message in every room, which made the badge pure
 * furniture. So a distinguished seat (the owner's title, the facilitator, the
 * secretary, a named role holder) is shown in the room's own words and marked;
 * a name the room knows nothing more about keeps its plain role. */
function RoleBadge({ seat }: { seat: Seat }) {
  if (seat.kind === "human" || !seat.title) return null
  const named = seat.kind === "seat" && !isGenericTitle(seat.title)
  return (
    <Badge
      className={cn(
        "h-[18px] rounded-full px-1.5 text-[9px] font-semibold uppercase tracking-wide",
        named
          ? "border-primary/40 bg-primary/10 text-primary"
          : "border-teal-200/70 bg-teal-50 text-teal-700",
      )}
      title={seat.label ? `seat ${seat.label}` : undefined}
      variant="outline"
    >
      {seat.title}
    </Badge>
  )
}

// EventRecord is the JSON half of the view toggle: the message's own record,
// exactly as this client received it.
//
// `raw` is left out and rendered below instead — inside a JSON string every
// newline is an escaped \n, so the one field a reader turns this view on to
// study would be the one field they could not read.
function EventRecord({ event }: { event: MeetEvent }) {
  const { raw: _raw, ...record } = event
  return (
    <pre
      className="mt-1 max-h-[420px] overflow-auto rounded-lg border border-border/70 bg-muted/40 px-2.5 py-2 text-[11px] leading-5 text-muted-foreground"
      data-event-json
    >
      {JSON.stringify(record, null, 2)}
    </pre>
  )
}

// RawTransport shows what the message above was extracted FROM.
//
// Collapsed by default even with the debug view on: a turn's transport is every
// tool call and every command output the agent produced, which is exactly the
// wall of JSON this seam exists to keep out of the conversation. The reader
// opens the one message they are debugging.
function RawTransport({ raw }: { raw: string }) {
  return (
    <details className="mt-2 rounded-lg border border-border/70 bg-muted/40">
      <summary className="cursor-pointer select-none px-2.5 py-1.5 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
        Raw transport ({raw.split("\n").length} lines)
      </summary>
      <pre className="max-h-[420px] overflow-auto px-2.5 pb-2.5 text-[11px] leading-5 text-muted-foreground">
        {raw}
      </pre>
    </details>
  )
}

function LiveMessage({
  live,
  state,
}: {
  live: LiveEvent
  state: State | null
}) {
  const seat = seatOf(live.speaker, state, live.role)
  const boundedProgress = live.status === "working"
  return (
    <article className="flex gap-3 rounded-xl bg-teal-50/45 px-2 py-4 sm:gap-4 sm:px-3">
      <Avatar className="mt-0.5 size-9 shrink-0 border border-teal-200 shadow-sm">
        <AvatarFallback className="bg-teal-100 text-teal-800">
          <Bot className="size-4" />
        </AvatarFallback>
      </Avatar>
      <div className="min-w-0 flex-1">
        <div className="mb-1 flex items-center gap-2">
          <SeatName seat={seat} />
          <Badge
            className="h-[18px] animate-pulse rounded-full border-teal-200 bg-teal-50 px-1.5 text-[9px] uppercase tracking-wide text-teal-700"
            variant="outline"
          >
            speaking
          </Badge>
        </div>
        {boundedProgress && (
          <div
            aria-live="polite"
            className="mb-2 flex flex-wrap items-center gap-x-2 text-[10px] font-medium uppercase tracking-wide text-teal-700/75"
            data-live-progress
          >
            <span>{(live.lines ?? 0).toLocaleString()} lines observed</span>
            <span aria-hidden="true">·</span>
            <span>{formatBytes(live.bytes ?? 0)}</span>
            <span aria-hidden="true">·</span>
            <span>{formatElapsed(live.elapsed_ms ?? 0)}</span>
          </div>
        )}
        {live.text ? (
          <pre className="max-h-44 overflow-auto whitespace-pre-wrap break-words font-mono text-[12px] leading-5 text-foreground/75">
            {live.text}
          </pre>
        ) : boundedProgress ? (
          <div
            aria-label={`${live.speaker} is typing`}
            className="text-[12px] text-muted-foreground"
          >
            Waiting for the first output line…
          </div>
        ) : (
          <div
            aria-label={`${live.speaker} is typing`}
            className="flex h-6 items-center gap-1"
          >
            {[0, 1, 2].map((item) => (
              <span
                className="typing-dot size-1.5 rounded-full bg-teal-600/55"
                key={item}
                style={{ animationDelay: `${item * 140}ms` }}
              />
            ))}
          </div>
        )}
      </div>
    </article>
  )
}

function formatBytes(bytes: number): string {
  if (bytes < 1_000) return `${bytes} B`
  if (bytes < 1_000_000) return `${(bytes / 1_000).toFixed(1)} kB`
  return `${(bytes / 1_000_000).toFixed(1)} MB`
}

function formatElapsed(ms: number): string {
  const seconds = Math.max(0, Math.floor(ms / 1_000))
  if (seconds < 60) return `${seconds}s elapsed`
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s elapsed`
}

function SystemEvent({
  event,
  debugRaw = false,
}: {
  event: MeetEvent
  debugRaw?: boolean
}) {
  const Icon = iconFor(event.kind)
  const highlighted = event.kind === "decision" || event.kind === "action"
  return (
    <div className="my-2 flex items-start gap-3 py-2 pl-[3.35rem] pr-3">
      <div
        className={cn(
          "mt-0.5 grid size-6 shrink-0 place-items-center rounded-full",
          highlighted
            ? "bg-amber-100 text-amber-700"
            : "bg-slate-100 text-slate-500",
        )}
      >
        <Icon className="size-3" />
      </div>
      <div className="min-w-0 flex-1">
        {debugRaw ? (
          <EventRecord event={event} />
        ) : (
          <p
            className={cn(
              "text-[12px] leading-5",
              highlighted
                ? "font-medium text-slate-700"
                : "text-muted-foreground",
            )}
          >
            {event.text || event.question}
          </p>
        )}
        <span className="text-[9px] uppercase tracking-wider text-muted-foreground/55">
          {event.kind} · {formatTime(event.ts)}
        </span>
      </div>
    </div>
  )
}

function iconFor(kind: MeetEvent["kind"]) {
  if (kind === "decision" || kind === "confirm") return CheckCircle2
  if (kind === "action") return ClipboardCheck
  if (kind === "agenda" || kind === "replan") return ListTodo
  if (kind === "invite" || kind === "kick") return UserRoundPlus
  if (kind === "poll" || kind === "vote") return Vote
  return CircleDotDashed
}

function formatTime(value?: string | number) {
  if (value === undefined) return ""
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ""
  return new Intl.DateTimeFormat(undefined, {
    hour: "numeric",
    minute: "2-digit",
  }).format(date)
}

/** stampOf is the record's own handle — the timestamp a retraction points at. */
function stampOf(event: MeetEvent): string {
  const ts = event.ts
  if (typeof ts === "string") return ts
  if (typeof ts === "number") return new Date(ts).toISOString()
  return ""
}

function eventKey(event: MeetEvent, index: number) {
  return `${event.kind}-${event.speaker}-${String(event.ts)}-${index}`
}
