import { useState } from "react"

import {
  Check,
  ClipboardList,
  Crown,
  FileText,
  Gavel,
  LockKeyhole,
  Timer,
  UserRoundMinus,
} from "lucide-react"

import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Separator } from "@/components/ui/separator"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import {
  memberName,
  memberRole,
  type AgentOption,
  type RoomDetail,
  type State,
} from "@/lib/contracts"
import { cn } from "@/lib/utils"

interface RoomDetailsProps {
  state: State | null
  agents: AgentOption[]
  detail: RoomDetail | null
  isOrganizer: boolean
  sending: boolean
  onAction: (action: string, label: string, body?: unknown) => Promise<boolean>
  className?: string
}

export function RoomDetails({
  agents,
  state,
  detail,
  isOrganizer,
  sending,
  onAction,
  className,
}: RoomDetailsProps) {
  const [invitee, setInvitee] = useState("")
  const [closeOpen, setCloseOpen] = useState(false)

  return (
    <aside
      className={cn(
        "flex h-full min-h-0 w-[320px] shrink-0 flex-col border-l border-border/70 bg-card/70",
        className,
      )}
    >
      {/* 56px, the same as the sidebar's and the conversation's own headers, so
          one unbroken rule runs under the app bar across all three panels. */}
      <div className="flex h-[56px] items-center px-5">
        <div>
          <div className="font-display text-[14px] font-semibold">
            Room details
          </div>
          <div className="text-[11px] text-muted-foreground">
            Shared memory and outcomes
          </div>
        </div>
      </div>
      <Separator />
      <ScrollArea className="min-h-0 flex-1">
        <div className="space-y-7 p-5">
          <section>
            <SectionTitle icon={ClipboardList}>Agenda</SectionTitle>
            <ol className="mt-3 space-y-3">
              {state?.agenda.map((item, index) => (
                <li className="flex gap-2.5 text-[12px] leading-5" key={item}>
                  <span className="grid size-5 shrink-0 place-items-center rounded-full bg-secondary text-[9px] font-bold text-muted-foreground">
                    {index + 1}
                  </span>
                  <span className="text-foreground/75">{item}</span>
                </li>
              ))}
            </ol>
          </section>

          <section>
            <SectionTitle icon={Check}>Decisions</SectionTitle>
            <div className="mt-3 space-y-2">
              {detail?.synthesis?.decisions?.map((item) => (
                <div
                  className="rounded-xl border border-amber-200/70 bg-amber-50/65 p-3 text-[12px] leading-5 text-amber-950/75"
                  key={item}
                >
                  {item}
                </div>
              )) || (
                <EmptyCopy>Decisions will collect here as the room agrees.</EmptyCopy>
              )}
            </div>
          </section>

          <section>
            <SectionTitle icon={FileText}>Minutes</SectionTitle>
            <div className="mt-3 space-y-3 border-l border-border pl-3">
              {detail?.synthesis?.minutes?.map((item) => (
                <p
                  className="text-[11px] leading-[1.65] text-muted-foreground"
                  key={item}
                >
                  {item}
                </p>
              )) || <EmptyCopy>No minutes yet.</EmptyCopy>}
            </div>
          </section>

          <section>
            <SectionTitle icon={Gavel}>Room setup</SectionTitle>
            <div className="mt-3 grid grid-cols-2 gap-2">
              <Meta label="Facilitator" value={state?.chair || "Not assigned"} />
              <Meta
                label="Secretary"
                value={
                  state?.secretary ||
                  (state?.secretary_pending
                    ? `Auto-select on first activity (L${state?.secretary_band || 2}+)`
                    : "Not assigned")
                }
              />
              <Meta
                icon={Timer}
                label="Turn limit"
                value={formatTurnTimeout(state?.turn_timeout)}
              />
              <Meta
                label="Decisions"
                value={state?.decision_mode || "Standard"}
              />
            </div>
          </section>

          <section className="rounded-xl border border-dashed border-border p-3.5">
            <div className="mb-2 flex items-center gap-2 text-[11px] font-semibold">
              {isOrganizer ? (
                <Crown className="size-3.5 text-amber-600" />
              ) : (
                <LockKeyhole className="size-3.5 text-muted-foreground" />
              )}
              Organizer controls
            </div>
            <p className="mb-3 text-[10px] leading-relaxed text-muted-foreground">
              {isOrganizer
                ? "You can manage membership and open or close this room. Closed permanent rooms retain their address and archived sessions."
                : `${state?.initiator || "The organizer"} manages membership and room closure.`}
            </p>
            {isOrganizer && (
              <>
                <form
                  className="mb-2 flex gap-2"
                  onSubmit={async (event) => {
                    event.preventDefault()
                    const name = invitee.trim()
                    if (!name) return
                    if (await onAction("invite", "Invite", { agent: name })) {
                      setInvitee("")
                    }
                  }}
                >
                  <select
                    aria-label="Agent to invite"
                    className="h-8 min-w-0 flex-1 rounded-md border border-input bg-transparent px-2 text-[12px]"
                    disabled={sending || state?.status === "closed"}
                    onChange={(event) => setInvitee(event.target.value)}
                    value={invitee}
                  >
                    <option value="">Invite an agent…</option>
                    {agents
                      .filter((agent) => !state?.participants.some((member) => memberName(member) === agent.name))
                      .map((agent) => (
                        <option key={agent.name} value={agent.name}>
                          {agent.nick ? `${agent.nick} · ` : ""}{agent.name}{agent.ephemeral ? " · temporary" : ""}
                        </option>
                      ))}
                  </select>
                  <Button
                    className="h-8 shrink-0 text-[12px]"
                    disabled={
                      sending || !invitee.trim() || state?.status === "closed"
                    }
                    size="sm"
                    type="submit"
                    variant="outline"
                  >
                    Invite
                  </Button>
                </form>
                <div className="mb-3 space-y-1" data-room-participants>
                  {state?.participants.map((member) => {
                    const name = memberName(member)
                    return (
                      <div
                        className="flex items-center gap-2 rounded-lg bg-secondary/50 px-2.5 py-1.5"
                        key={name}
                      >
                        <span className="min-w-0 flex-1 truncate text-[11px]">
                          {name}
                          {memberRole(member) && (
                            <span className="ml-1 text-muted-foreground">
                              · {memberRole(member)}
                            </span>
                          )}
                        </span>
                        <Button
                          aria-label={`Remove ${name}`}
                          className="size-6 text-muted-foreground hover:text-destructive"
                          disabled={sending || state?.status === "closed"}
                          onClick={() =>
                            onAction("kick", `Remove ${name}`, { agent: name })
                          }
                          size="icon-sm"
                          type="button"
                          variant="ghost"
                        >
                          <UserRoundMinus className="size-3.5" />
                        </Button>
                      </div>
                    )
                  })}
                </div>
              </>
            )}
            <div className="flex gap-2">
              <OrganizerButton
                disabled={
                  sending ||
                  !isOrganizer
                }
                label={
                  state?.status === "closed" ? "Open room" : "Close room"
                }
                onClick={() => {
                  if (state?.status === "closed") {
                    void onAction("open", "Open room")
                  } else {
                    setCloseOpen(true)
                  }
                }}
                unavailableReason={
                  sending
                    ? "Another room action is in progress."
                    : "Only the organizer can use this control."
                }
              />
            </div>
            <Dialog onOpenChange={setCloseOpen} open={closeOpen}>
              <DialogContent className="sm:max-w-[420px]">
                <DialogHeader>
                  <DialogTitle>Close this room?</DialogTitle>
                  <DialogDescription>
                    This files the minutes and ends the conversation. The
                    transcript remains available, but the room cannot accept
                    more messages or turns.
                  </DialogDescription>
                </DialogHeader>
                <DialogFooter>
                  <Button
                    disabled={sending}
                    onClick={() => setCloseOpen(false)}
                    type="button"
                    variant="ghost"
                  >
                    Keep open
                  </Button>
                  <Button
                    disabled={sending}
                    onClick={async () => {
                      if (await onAction("close", "Close room")) {
                        setCloseOpen(false)
                      }
                    }}
                    type="button"
                    variant="destructive"
                  >
                    {sending ? "Closing…" : "Close room"}
                  </Button>
                </DialogFooter>
              </DialogContent>
            </Dialog>
          </section>
        </div>
      </ScrollArea>
    </aside>
  )
}

function SectionTitle({
  icon: Icon,
  children,
}: {
  icon: typeof ClipboardList
  children: React.ReactNode
}) {
  return (
    <h2 className="flex items-center gap-2 text-[10px] font-bold uppercase tracking-[0.14em] text-muted-foreground">
      <Icon className="size-3.5" />
      {children}
    </h2>
  )
}

/**
 * State.turn_timeout is a Go duration marshalled as a STRING — "20m", "1h30m",
 * "45s" — not a count of seconds. Appending "s" to it rendered the default 20
 * MINUTE turn limit as "20ms", which is not a rounding error but three orders
 * of magnitude and the wrong unit, on the one field that tells a human how long
 * an agent may hold the floor.
 *
 * A number is still accepted (contracts.ts allows either) and read as seconds.
 */
function formatTurnTimeout(value: string | number | undefined): string {
  if (value === undefined || value === "") return "Default"
  if (typeof value === "number") return `${value}s`
  return value
}

function Meta({
  label,
  value,
  icon: Icon,
}: {
  label: string
  value: string
  icon?: typeof Timer
}) {
  return (
    <div className="rounded-lg bg-secondary/60 p-2.5">
      <div className="mb-1 flex items-center gap-1 text-[9px] uppercase tracking-wider text-muted-foreground">
        {Icon && <Icon className="size-2.5" />}
        {label}
      </div>
      <div className="truncate text-[11px] font-medium capitalize">{value}</div>
    </div>
  )
}

function OrganizerButton({
  disabled,
  label,
  onClick,
  unavailableReason,
}: {
  disabled: boolean
  label: string
  onClick: () => void
  unavailableReason: string
}) {
  const button = (
    <span className="inline-flex">
      <Button
        className="h-7 text-[10px]"
        disabled={disabled}
        onClick={onClick}
        size="sm"
        variant="outline"
      >
        {label}
      </Button>
    </span>
  )
  if (!disabled) return button
  return (
    <Tooltip>
      <TooltipTrigger asChild>{button}</TooltipTrigger>
      <TooltipContent>{unavailableReason}</TooltipContent>
    </Tooltip>
  )
}

function EmptyCopy({ children }: { children: React.ReactNode }) {
  return (
    <p className="text-[11px] italic leading-5 text-muted-foreground">
      {children}
    </p>
  )
}
