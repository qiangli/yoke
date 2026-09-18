import { useState } from "react"
import {
  Braces,
  ChevronRight,
  Menu,
  PanelRightClose,
  PanelRightOpen,
  Users,
} from "lucide-react"

import { Composer } from "@/components/composer"
import { MessageList } from "@/components/message-list"
import { RoomDetails } from "@/components/room-details"
import { RoomSidebar } from "@/components/room-sidebar"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Sheet,
  SheetContent,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet"
import { TooltipProvider } from "@/components/ui/tooltip"
import { useMeetRoom } from "@/hooks/use-meet-room"

export function App() {
  const meet = useMeetRoom()
  const [detailsOpen, setDetailsOpen] = useState(true)
  const [mobileDetailsOpen, setMobileDetailsOpen] = useState(false)
  const memberCount = meet.state?.participants.length ?? 0

  return (
    <TooltipProvider>
      {/* Four parts: one bar across the top, then left / middle / right beneath
          it — the shape board, messages and terminal have. relay used to be
          three columns with the app bar living INSIDE the middle one, so the
          header began 280px in and the app carried two brands. */}
      <div className="flex h-dvh min-h-[540px] flex-col overflow-hidden bg-background text-foreground">
        <header className="console-bar shrink-0">
          <a className="console-brand" href="./" title="bashy meet">
            {/* Themed like every other app's mark: --logo-* invert between
                light and dark, so the tile never sits as a dark block in a
                light header. */}
            <svg className="console-logo" viewBox="0 0 40 40" aria-hidden="true">
              <rect width="40" height="40" rx="10" fill="var(--logo-bg)" />
              <path
                d="M10 12h20v13H17l-6 5v-5h-1z"
                fill="none"
                stroke="var(--logo-fg)"
                strokeLinejoin="round"
                strokeWidth="2.5"
              />
              <path
                d="M15 18h10"
                stroke="var(--logo-accent)"
                strokeLinecap="round"
                strokeWidth="2.5"
              />
            </svg>
            <span className="console-wordmark hidden sm:flex">
              bashy<b>meet</b>
            </span>
          </a>

          <span className="flex-1" />

          {/* The console's all-apps mark. relay is MOUNTED, served outside the
              path that injects #all-apps-btn, so it draws the same mark. */}
          <a className="console-iconbtn" href="../" title="All apps" aria-label="Back to all apps">
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.9"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <rect x="3" y="3" width="7" height="7" rx="1.5" />
              <rect x="14" y="3" width="7" height="7" rx="1.5" />
              <rect x="3" y="14" width="7" height="7" rx="1.5" />
              <rect x="14" y="14" width="7" height="7" rx="1.5" />
            </svg>
          </a>
        </header>

        <div className="flex min-h-0 flex-1 overflow-hidden">
        <RoomSidebar
          agents={meet.agents}
          className="hidden lg:flex"
          connection={meet.connection}
          creating={meet.creating}
          dms={meet.dms}
          onCreateDM={meet.createDM}
          onCreate={meet.createRoom}
          onSelect={meet.selectRoom}
          onSelectDM={meet.selectDM}
          onModeChange={meet.selectMode}
          rooms={meet.rooms}
          selectedRef={meet.selectedRef}
          selectedKind={meet.selectedKind}
          viewKind={meet.viewKind}
          state={meet.state}
          usingMock={meet.usingMock}
        />

        <main className="flex min-w-0 flex-1 flex-col">
          {/* The conversation's own sub-header. The APP bar is above this,
              spanning all three panels. */}
          <header className="flex h-[56px] shrink-0 items-center gap-3 border-b border-border/70 px-3 sm:px-5">
            <Sheet>
              <SheetTrigger asChild>
                <Button
                  aria-label="Open rooms and members"
                  className="lg:hidden"
                  size="icon-sm"
                  variant="ghost"
                >
                  <Menu />
                </Button>
              </SheetTrigger>
              <SheetContent
                className="w-[300px] border-0 p-0"
                side="left"
                showCloseButton={false}
              >
                <SheetTitle className="sr-only">Rooms and members</SheetTitle>
                <RoomSidebar
                  agents={meet.agents}
                  className="w-full"
                  connection={meet.connection}
                  creating={meet.creating}
                  dms={meet.dms}
                  onCreateDM={meet.createDM}
                  onCreate={meet.createRoom}
                  onSelect={meet.selectRoom}
                  onSelectDM={meet.selectDM}
                  onModeChange={meet.selectMode}
                  rooms={meet.rooms}
                  selectedRef={meet.selectedRef}
                  selectedKind={meet.selectedKind}
                  viewKind={meet.viewKind}
                  state={meet.state}
                  usingMock={meet.usingMock}
                />
              </SheetContent>
            </Sheet>

            <div className="min-w-0 flex-1">
              <div className="flex min-w-0 items-center gap-2">
                <h1 className="font-display truncate text-[15px] font-semibold tracking-tight sm:text-[17px]">
                  {meet.state?.name || meet.state?.room || "Opening conversation…"}
                </h1>
                {meet.usingMock && (
                  <Badge
                    className="hidden h-[18px] rounded-full border-violet-200 bg-violet-50 px-1.5 text-[9px] uppercase tracking-wide text-violet-700 sm:inline-flex"
                    variant="outline"
                  >
                    demo
                  </Badge>
                )}
              </div>
              <div className="flex items-center gap-1 text-[10px] text-muted-foreground sm:text-[11px]">
                <span className="max-w-[52vw] truncate">
                  {meet.state?.topic || "Connecting to the conversation"}
                </span>
                <ChevronRight className="size-3" />
                <Users className="size-3" />
                <span>{memberCount}</span>
              </div>
            </div>

            <Button
              aria-label={
                meet.debugRaw
                  ? "Show messages as text"
                  : "Show messages as JSON"
              }
              aria-pressed={meet.debugRaw}
              className="h-7 px-2 text-[10px] font-semibold uppercase tracking-wide"
              onClick={() => meet.setDebugRaw(!meet.debugRaw)}
              size="sm"
              title="Switch each message between its prose and the JSON record it was rendered from"
              variant={meet.debugRaw ? "secondary" : "ghost"}
            >
              <Braces className="size-3.5" />
              JSON
            </Button>

            {meet.selectedKind === "room" && <Button
              aria-label={detailsOpen ? "Hide room details" : "Show room details"}
              className="hidden xl:inline-flex"
              onClick={() => setDetailsOpen((open) => !open)}
              size="icon-sm"
              variant="ghost"
            >
              {detailsOpen ? <PanelRightClose /> : <PanelRightOpen />}
            </Button>}

            {meet.selectedKind === "room" && <Sheet onOpenChange={setMobileDetailsOpen} open={mobileDetailsOpen}>
              <SheetTrigger asChild>
                <Button
                  aria-label="Open room details"
                  className="xl:hidden"
                  size="icon-sm"
                  variant="outline"
                >
                  <PanelRightOpen />
                </Button>
              </SheetTrigger>
              <SheetContent
                className="w-[340px] max-w-[92vw] p-0"
                side="right"
              >
                <SheetTitle className="sr-only">Room details</SheetTitle>
                <RoomDetails
                  agents={meet.agents}
                  className="w-full border-0"
                  detail={meet.detail}
                  isOrganizer={meet.isOrganizer}
                  onAction={meet.act}
                  sending={meet.sending}
                  state={meet.state}
                />
              </SheetContent>
            </Sheet>}

          </header>

          <MessageList
            debugRaw={meet.debugRaw}
            events={meet.events}
            kind={meet.selectedKind}
            live={meet.live}
            state={meet.state}
          />
          <Composer
            error={meet.error}
            initialDraft={meet.draft}
            onDismissQueued={meet.dismissQueued}
            onRecipientChange={meet.setRecipient}
            onSend={meet.send}
            queued={meet.queued}
            recipient={meet.recipient}
            sending={meet.sending}
            state={meet.state}
            kind={meet.selectedKind}
          />
        </main>

        {detailsOpen && meet.selectedKind === "room" && (
          <RoomDetails
            agents={meet.agents}
            className="hidden xl:flex"
            detail={meet.detail}
            isOrganizer={meet.isOrganizer}
            onAction={meet.act}
            sending={meet.sending}
            state={meet.state}
          />
        )}
        </div>

        {/* Relay is mounted rather than injected, so it states the same shared
            copyright line directly. Keep this text synchronized with
            chromeCopyrightHTML in webconsole/embed.go. */}
        <footer className="console-foot">
          <span id="copyright">&copy; 2026 qiangli. All rights reserved.</span>
        </footer>
      </div>
    </TooltipProvider>
  )
}
