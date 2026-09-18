import {
  ALL_SEATS,
  memberName,
  memberRole,
  type MeetEvent,
  type State,
} from "@/lib/contracts"

/** A Seat is a NAME AS THIS ROOM KNOWS IT — who they are, and what they are
 * called HERE.
 *
 * It exists because a transcript event carries only `role`, and every agent
 * turn in every room is recorded with the same one: `participant`. That is the
 * meeting's role (it decides CONTENT — see session.go), not the reader's
 * question. A reader looking at a sprint room wants to know which of these
 * equal-looking names is the project manager, which is the facilitator, and
 * which is holding a named seat like `steward` — and every one of those facts
 * is on State, one indirection away from the event.
 *
 * So the resolution happens once, here, and the message list, the composer and
 * the roster all read it. Three surfaces that disagree about who is
 * accountable are worse than three that say nothing.
 */
export interface Seat {
  /** The bare name — or the seat label itself when the seat is vacant. */
  name: string
  /** How a composer ADDRESSES it: "@codex", "everyone", "the room". */
  handle: string
  /** What it is CALLED here: "project manager", "facilitator", "you", … */
  title: string
  kind: "human" | "everyone" | "room" | "seat" | "agent"
  /** The stored late-bound label ("conductor:99") when the address was one. */
  label?: string
}

/** Titles that say nothing a reader did not already know. A seat whose title
 * is one of these is not worth a second line next to its name — and it is what
 * separates "@codex · project manager" from a badge that reads "participant"
 * beside every name in the room. */
const GENERIC = new Set(["", "agent", "participant", "assistant", "role"])

export function isGenericTitle(title: string): boolean {
  return GENERIC.has(title.trim().toLocaleLowerCase())
}

function eq(a: string | undefined, b: string | undefined): boolean {
  if (!a || !b) return false
  return a.trim().toLocaleLowerCase() === b.trim().toLocaleLowerCase()
}

function bare(name: string): string {
  return name.trim().replace(/^@/, "")
}

function isEveryone(name: string): boolean {
  return bare(name).toLocaleLowerCase() === ALL_SEATS
}

/** titleForLabel names the seat a role label addresses, the way the server
 * does (meet/owner.go titleForRoleLabel): `conductor:99` is a SPRINT's seat and
 * the domain's word for its owner is "project manager". Anything else keeps its
 * own label, because inventing a title for a seat this UI does not own would be
 * a guess presented as a fact. */
export function titleForLabel(label: string): string {
  const kind = label.includes(":") ? label.slice(0, label.indexOf(":")) : label
  const lower = kind.trim().toLocaleLowerCase()
  return lower === "conductor" ? "project manager" : lower
}

const ROOM: Seat = {
  name: "",
  handle: "the room",
  title: "room",
  kind: "room",
}

const EVERYONE: Seat = {
  name: ALL_SEATS,
  handle: "everyone",
  title: "every seat in the room",
  kind: "everyone",
}

function agentSeat(name: string, title: string, kind: Seat["kind"], label?: string): Seat {
  return { name, handle: `@${name}`, title, kind, label }
}

/** seatOf resolves one name against the room, most specific fact first.
 *
 * The order is the point. A room's OWNER outranks its facilitator (a sprint's
 * room advertises for the conductor's seat, and unaddressed mail lands there
 * server-side), and a named role holder outranks both, because that is the name
 * the room is addressed BY.
 *
 * `fallbackRole` is the event's own recorded role, used only when the room
 * knows nothing more specific.
 */
export function seatOf(
  rawName: string | undefined,
  state: State | null,
  fallbackRole?: string,
): Seat {
  const name = bare(rawName ?? "")
  if (!name) return ROOM
  if (isEveryone(name)) return EVERYONE

  const role = (fallbackRole ?? "").trim().toLocaleLowerCase()
  if (eq(name, state?.human) || role === "human" || role === "user") {
    // The human reading this page. Not "@addressable": meet routes seats, and
    // a person is not one — they post on their own behalf.
    return { name, handle: name, title: "you", kind: "human" }
  }

  // A named seat ("steward"), resolved in either direction: the transcript may
  // carry the HOLDER's name while the room is addressed by the ALIAS.
  for (const [alias, holder] of Object.entries(state?.role_holders ?? {})) {
    if (eq(holder, name)) return agentSeat(name, titleForLabel(alias), "seat", alias)
    if (eq(alias, name)) return agentSeat(holder || name, titleForLabel(alias), "seat", alias)
  }

  if (eq(name, state?.owner)) {
    return agentSeat(name, state?.owner_title || "owner", "seat", state?.default_to || undefined)
  }
  if (eq(name, state?.chair)) return agentSeat(name, "facilitator", "seat")
  if (eq(name, state?.secretary)) return agentSeat(name, "secretary", "seat")

  const member = (state?.participants ?? []).find((m) => eq(memberName(m), name))
  const memberTitle = member ? memberRole(member) : undefined
  return agentSeat(name, (memberTitle || role || "agent").toLocaleLowerCase(), "agent")
}

/** addresseeOf answers "and who is this FOR" for one transcript event.
 *
 * Three real answers, and the third is not a shrug:
 *
 *   - a name or a seat label — directed mail, which lands in that seat's inbox;
 *   - `all` — an explicit broadcast, in EVERY participant's inbox;
 *   - nobody — shared room history. An agent's TURN is deliberately in this
 *     bucket: "the agent's reply names nobody, which is what stops a reply from
 *     waking anything and cascading" (relay_dm.go). So this returns the room
 *     rather than inventing an addressee from the message it appears to answer;
 *     a guess rendered as a fact is the failure this file exists to avoid.
 */
export function addresseeOf(event: MeetEvent, state: State | null): Seat {
  const to = bare(event.to ?? "")
  if (!to) return ROOM
  if (isEveryone(to)) return EVERYONE
  // The room's own late-bound address. The SERVER resolved who holds it — the
  // label is stored, never the holder — so show the holder and keep the label
  // for the tooltip.
  if (state?.default_to && eq(to, state.default_to)) {
    return {
      name: state.owner || to,
      handle: state.owner ? `@${state.owner}` : to,
      title: state.owner_title || titleForLabel(to),
      kind: "seat",
      label: to,
    }
  }
  return seatOf(to, state)
}
