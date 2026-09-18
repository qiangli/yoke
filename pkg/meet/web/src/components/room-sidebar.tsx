import { useEffect, useState } from "react"
import {
  Bot,
  ChevronRight,
  Hash,
  Plus,
  Radio,
  UserRound,
} from "lucide-react"

import { NewRoomDialog } from "@/components/new-room-dialog"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { Badge } from "@/components/ui/badge"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Separator } from "@/components/ui/separator"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import type { ConnectionStatus } from "@/hooks/use-meet-room"
import {
  memberIsLive,
  memberName,
  memberRole,
  type Member,
  type RoomSummary,
  type AgentOption,
  type DMSummary,
  type State,
} from "@/lib/contracts"
import { seatOf } from "@/lib/seats"
import { cn } from "@/lib/utils"

interface RoomSidebarProps {
  creating: boolean
  agents: AgentOption[]
  onCreate: (topic: string, owner: string, participants: string[]) => Promise<boolean>
  rooms: RoomSummary[]
  dms: DMSummary[]
  selectedRef: string
  selectedKind: "room" | "dm"
  viewKind: "room" | "dm"
  state: State | null
  connection: ConnectionStatus
  usingMock: boolean
  onSelect: (ref: string) => void
  onSelectDM: (agent: string) => void
  onCreateDM: (agent: string) => Promise<boolean>
  onModeChange: (kind: "room" | "dm") => void
  className?: string
}

/** rosterOf is who to show under "In this room".
 *
 * The participants, plus the OWNER when it is not among them. A meeting's
 * facilitator runs the floor without being seated, and a sprint room's seat
 * holder may not have spoken yet — so the roster could list every agent in the
 * room and omit the one person accountable for it, which is the opposite of
 * what a roster is for.
 */
function rosterOf(state: State | null): Member[] {
  const participants = state?.participants ?? []
  const owner = state?.owner ?? ""
  if (!owner) return participants
  const seated = participants.some(
    (member) => memberName(member).toLocaleLowerCase() === owner.toLocaleLowerCase(),
  )
  if (seated) return participants
  return [{ name: owner, role: state?.owner_title || "owner", live: false }, ...participants]
}

export function RoomSidebar({
  agents,
  creating,
  onCreate,
  rooms,
  dms,
  selectedRef,
  selectedKind,
  viewKind,
  state,
  connection,
  usingMock,
  onSelect,
  onSelectDM,
  onCreateDM,
  onModeChange,
  className,
}: RoomSidebarProps) {
  const [newRoomOpen, setNewRoomOpen] = useState(false)
  const [dmMenuOpen, setDMMenuOpen] = useState(false)

  // The Sprint page's New sprint shortcut opens Chat without choosing an
  // agent on the user's behalf. Put the existing agent picker in front of the
  // reader immediately; after they choose, the prefilled draft becomes an
  // ordinary editable DM input.
  useEffect(() => {
    if (viewKind === "dm" && selectedKind === "dm" && !selectedRef) {
      setDMMenuOpen(true)
    }
  }, [viewKind, selectedKind, selectedRef])

  function openChannels() {
    onModeChange("room")
    if (rooms[0]) onSelect(rooms[0].id)
    else window.setTimeout(() => setNewRoomOpen(true), 0)
  }

  function openDirectMessages() {
    onModeChange("dm")
    if (dms[0]) onSelectDM(dms[0].agent)
    else window.setTimeout(() => setDMMenuOpen(true), 0)
  }

  return (
    <aside
      className={cn(
        "flex h-full min-h-0 w-[280px] shrink-0 flex-col bg-sidebar text-sidebar-foreground",
        className,
      )}
    >
      {/* 56px to match the conversation sub-header beside it, so one unbroken
          rule runs under the app bar across all three panels.

          The trigger used to BE the app logo. That put a second copy of the
          mark directly under the one in the app bar, and made the control look
          like branding rather than the mode switch it is. It now says which
          mode is open — Meet or Chat — which is the thing worth reading here. */}
      {/* Meet and Chat are two views of this panel, so they are TABS, not a
          menu: both are visible, the selected one is obvious without opening
          anything, and switching is one tap rather than two. It was a dropdown
          hanging off the app logo, which hid the choice behind a mark that
          looked like branding.

          56px to match the conversation and details headers beside it, so one
          unbroken rule runs under the app bar. */}
      <div className="flex h-[56px] items-center gap-2 px-4">
        <div
          className="flex items-center gap-0.5 rounded-lg bg-sidebar-accent/60 p-0.5"
          role="tablist"
          aria-label="Meet or Chat"
        >
          <button
            aria-selected={viewKind === "room"}
            className={cn(
              "flex items-center gap-1.5 rounded-md px-2.5 py-1 text-[13px] font-medium transition",
              viewKind === "room"
                ? "bg-sidebar text-sidebar-foreground shadow-sm"
                : "text-sidebar-foreground/60 hover:text-sidebar-foreground",
            )}
            onClick={openChannels}
            role="tab"
            type="button"
          >
            <Hash className="size-3.5" /> Meet
          </button>
          <button
            aria-selected={viewKind === "dm"}
            className={cn(
              "flex items-center gap-1.5 rounded-md px-2.5 py-1 text-[13px] font-medium transition",
              viewKind === "dm"
                ? "bg-sidebar text-sidebar-foreground shadow-sm"
                : "text-sidebar-foreground/60 hover:text-sidebar-foreground",
            )}
            onClick={openDirectMessages}
            role="tab"
            type="button"
          >
            <Bot className="size-3.5" /> Chat
          </button>
        </div>

        <span className="flex-1" />

        <span
          className="flex items-center gap-1.5 text-[11px] text-sidebar-foreground/55"
          title={usingMock ? "Demo workspace" : connection}
        >
          <span
            className={cn(
              "size-1.5 rounded-full",
              connection === "open" ? "bg-emerald-400" : "bg-amber-400",
            )}
          />
        </span>
      </div>
      <Separator className="bg-sidebar-border" />

      <ScrollArea className="min-h-0 flex-1">
        <div className="px-3 py-5">
          {viewKind === "room" ? <><div className="mb-2 flex items-center justify-between px-2 text-[11px] font-semibold uppercase tracking-[0.14em] text-sidebar-foreground/45">
            <span>Meetings</span>
            <div className="flex items-center gap-1">
              <NewRoomDialog agents={agents} creating={creating} onCreate={onCreate} open={newRoomOpen} onOpenChange={setNewRoomOpen} />
            </div>
          </div>
          <div className="space-y-1">
            {rooms.map((room) => {
              const selected = selectedKind === "room" && room.id === selectedRef
              return (
                <button
                  className={cn(
                    "group flex w-full items-center gap-2.5 rounded-lg px-2.5 py-2.5 text-left transition-colors",
                    selected
                      ? "bg-sidebar-accent text-sidebar-accent-foreground"
                      : "text-sidebar-foreground/70 hover:bg-sidebar-accent/55 hover:text-sidebar-foreground",
                  )}
                  key={room.id}
                  onClick={() => onSelect(room.id)}
                  type="button"
                >
                  <Hash
                    className={cn(
                      "size-4 shrink-0",
                      selected
                        ? "text-sidebar-primary"
                        : "text-sidebar-foreground/40",
                    )}
                  />
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-[13px] font-medium">
                      {room.name || room.room || room.topic}
                    </span>
                    <span className="block truncate text-[11px] text-sidebar-foreground/45">
                      {room.permanent ? `Permanent · ${room.topic}` : room.topic}
                    </span>
                  </span>
                  {selected && (
                    <ChevronRight className="size-3.5 text-sidebar-foreground/45" />
                  )}
                </button>
              )
            })}
          </div></> : <><div className="mb-2 flex items-center justify-between px-2 text-[11px] font-semibold uppercase tracking-[0.14em] text-sidebar-foreground/45">
            <span>Past conversations</span>
            <DropdownMenu onOpenChange={setDMMenuOpen} open={dmMenuOpen}>
              <DropdownMenuTrigger asChild>
                <Button aria-label="Start a direct message" className="size-6 text-sidebar-foreground/55" size="icon" variant="ghost">
                  <Plus className="size-3.5" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="max-h-72 w-64 overflow-y-auto">
                {agents.map((agent) => (
                  <DropdownMenuItem key={agent.name} onSelect={() => void onCreateDM(agent.name)}>
                    <Bot className="size-3.5" /> {agent.nick || agent.name}
                    {agent.nick && <span className="ml-auto text-[10px] text-muted-foreground">{agent.name}</span>}
                  </DropdownMenuItem>
                ))}
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
          <div className="space-y-1">
            {dms.map((dm) => {
              const selected = selectedKind === "dm" && dm.agent === selectedRef
              return (
                <button className={cn("flex w-full items-center gap-2.5 rounded-lg px-2.5 py-2 text-left", selected ? "bg-sidebar-accent text-sidebar-accent-foreground" : "text-sidebar-foreground/70 hover:bg-sidebar-accent/55")} key={dm.agent} onClick={() => onSelectDM(dm.agent)} type="button">
                  <Bot className="size-4 text-teal-300" />
                  <span className="truncate text-[13px] font-medium">{dm.agent}</span>
                </button>
              )
            })}
          </div>
          {dms.length === 0 && <p className="px-2 py-3 text-[11px] leading-5 text-sidebar-foreground/45">No chats yet. Start one with a registered agent.</p>}
          </>}

          {viewKind === "room" && selectedKind === "room" && state && <><div className="mb-2 mt-7 flex items-center justify-between px-2 text-[11px] font-semibold uppercase tracking-[0.14em] text-sidebar-foreground/45">
            <span>In this room</span>
            <Radio className="size-3.5" />
          </div>
          <div className="space-y-1">
            {rosterOf(state).map((member) => {
              const name = memberName(member)
              const role = memberRole(member)
              const human = role === "human" || name === state.human
              // The room's OWNER — its project manager when the room belongs to
              // a sprint. The server resolves the seat; the roster only has to
              // point at whoever holds it, which is the one thing a reader
              // could not work out from a list of equal-looking agents.
              const owner =
                Boolean(state.owner) &&
                name.toLocaleLowerCase() === state.owner.toLocaleLowerCase()
              return (
                <div
                  className="flex items-center gap-2.5 rounded-lg px-2.5 py-2"
                  key={name}
                >
                  <div className="relative">
                    <Avatar className="size-7 border border-white/10">
                      <AvatarFallback
                        className={cn(
                          "text-[10px] font-bold",
                          human
                            ? "bg-violet-400/20 text-violet-200"
                            : "bg-teal-400/20 text-teal-100",
                        )}
                      >
                        {human ? (
                          <UserRound className="size-3.5" />
                        ) : (
                          <Bot className="size-3.5" />
                        )}
                      </AvatarFallback>
                    </Avatar>
                    {memberIsLive(member) && (
                      <span className="absolute -bottom-px -right-px size-2.5 rounded-full border-2 border-sidebar bg-emerald-400" />
                    )}
                  </div>
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-[12px] font-medium">{name}</div>
                    {/* The owner's title goes in the ROLE line rather than in a
                        second badge beside it: a 280px row that says
                        "Project Manager" twice spends its width restating
                        itself. Colour carries the emphasis instead. */}
                    <div
                      className={cn(
                        "truncate text-[10px] capitalize",
                        owner
                          ? "font-semibold text-sidebar-primary"
                          : "text-sidebar-foreground/42",
                      )}
                    >
                      {/* One resolution for all three surfaces (see lib/seats):
                          the roster, the composer's recipient list and the
                          transcript must not disagree about who the project
                          manager, the facilitator or a named seat holder is. */}
                      {human ? "You" : seatOf(name, state, role).title}
                    </div>
                  </div>
                  {!owner && name === state.initiator && (
                    <Badge
                      className="shrink-0 border-white/10 bg-white/5 px-1.5 py-0 text-[9px] text-sidebar-foreground/55"
                      variant="outline"
                    >
                      host
                    </Badge>
                  )}
                </div>
              )
            })}
          </div></>}
        </div>
      </ScrollArea>
      <div className="border-t border-sidebar-border px-5 py-3.5 text-[10px] leading-relaxed text-sidebar-foreground/38">
        {viewKind === "room" ? "Meet · shared rooms and outcomes." : "Chat · private one-to-one conversations."}
      </div>
    </aside>
  )
}
