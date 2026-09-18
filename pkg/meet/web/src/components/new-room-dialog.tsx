import { useState, type FormEvent } from "react"
import { Plus } from "lucide-react"

import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import type { AgentOption } from "@/lib/contracts"

interface NewRoomDialogProps {
  creating: boolean
  agents: AgentOption[]
  onCreate: (topic: string, owner: string, participants: string[]) => Promise<boolean>
  open?: boolean
  onOpenChange?: (open: boolean) => void
}

/**
 * Opening a room is the one thing the browser could not do: every room had to
 * be started from the CLI, which left the web surface able to join a
 * conversation but never to begin one.
 *
 * Relay distinguishes a governed one-agent Chat from a Meet room. A meeting
 * may begin with one agent and grow, but it owns shared roster/outcome/minutes
 * semantics from creation; it is never stored as a Chat conversation.
 */
export function NewRoomDialog({ agents, creating, onCreate, open: controlledOpen, onOpenChange }: NewRoomDialogProps) {
  const [internalOpen, setInternalOpen] = useState(false)
  const open = controlledOpen ?? internalOpen
  const setOpen = onOpenChange ?? setInternalOpen
  const [topic, setTopic] = useState("")
  const [owner, setOwner] = useState("")
  const [participants, setParticipants] = useState<string[]>([])
  const ready = topic.trim().length > 0 && owner.length > 0 && participants.length > 0

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (!ready || creating) return
    if (await onCreate(topic.trim(), owner, participants)) {
      setTopic("")
      setOwner("")
      setParticipants([])
      setOpen(false)
    }
    // On failure the dialog stays open with the input intact — the error is
    // rendered in the room view, and discarding what was typed would make a
    // transient failure cost the user their message.
  }

  return (
    <Dialog onOpenChange={setOpen} open={open}>
      <DialogTrigger asChild>
        <Button
          className="h-7 gap-1 px-2 text-[11px] text-sidebar-foreground/75 hover:text-sidebar-foreground"
          size="sm"
          variant="ghost"
        >
          <Plus className="size-3.5" /> New meeting
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-[440px]">
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>New meeting</DialogTitle>
            <DialogDescription>
              Meet is for a shared room with membership, roles, outcomes, and
              optional minutes. For a plain one-to-one conversation, use Chat.
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-4 py-5">
            <div className="grid gap-1.5">
              <label className="text-[13px] font-medium" htmlFor="room-topic">
                Topic
              </label>
              <Input
                autoFocus
                id="room-topic"
                onChange={(event) => setTopic(event.target.value)}
                placeholder="What is this room about?"
                value={topic}
              />
            </div>
            <div className="grid gap-1.5">
              <label className="text-[13px] font-medium" htmlFor="room-owner">
                Facilitator
              </label>
              <select
                className="h-9 rounded-md border border-input bg-transparent px-3 text-[12px] outline-none focus-visible:ring-2 focus-visible:ring-ring"
                id="room-owner"
                onChange={(event) => {
                  const next = event.currentTarget.value
                  setOwner(next)
                  setParticipants((current) => current.filter((name) => name !== next))
                }}
                value={owner}
              >
                <option value="">Choose a registered agent…</option>
                {agents.map((agent) => (
                  <option key={agent.name} value={agent.name}>
                    {agent.nick ? `${agent.nick} · ` : ""}{agent.name}{agent.available ? "" : " · unavailable here"}
                  </option>
                ))}
              </select>
              <p className="text-[11px] text-muted-foreground">
                Required. Meet never chooses a facilitator for you.
              </p>
            </div>
            <div className="grid gap-1.5">
              <label className="text-[13px] font-medium" htmlFor="room-agents">
                Participants
              </label>
              <select
                className="min-h-32 rounded-md border border-input bg-transparent px-3 py-2 text-[12px] outline-none focus-visible:ring-2 focus-visible:ring-ring"
                id="room-agents"
                multiple
                onChange={(event) => setParticipants(Array.from(event.currentTarget.selectedOptions, (option) => option.value))}
                value={participants}
              >
                {agents.filter((agent) => agent.name !== owner).map((agent) => (
                  <option key={agent.name} value={agent.name}>
                    {agent.nick ? `${agent.nick} · ` : ""}{agent.name}{agent.band ? ` · L${agent.band}` : ""}{agent.ephemeral ? " · temporary" : ""}{agent.available ? "" : " · unavailable here"}
                  </option>
                ))}
              </select>
              <p className="text-[11px] text-muted-foreground">
                Choose registered agents. Use Ctrl/⌘ to select several.
              </p>
            </div>
          </div>

          <DialogFooter>
            <Button
              onClick={() => setOpen(false)}
              type="button"
              variant="ghost"
            >
              Cancel
            </Button>
            <Button disabled={!ready || creating} type="submit">
              {creating ? "Opening…" : "Open meeting"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
