import { useEffect, useRef, useState } from "react"
import { Terminal } from "@xterm/xterm"
import "@xterm/xterm/css/xterm.css"

// The agent's own terminal, mirrored read-only.
//
// The server replays the live seat's raw PTY capture and follows it
// (pkg/meet/console.go), so this shows exactly what the operator's terminal
// shows — every turn and every intermediate stream, at the size it was drawn
// for. It is OUTPUT only: the xterm takes no keystrokes, and steering stays
// on the composer below, which reaches the same session through its control
// socket.
//
// The TUI is never reflowed to the viewport (a TUI redrawn at another width is
// garbage); the font shrinks to fit the host's columns instead, and anything
// still wider scrolls.

type Status = "connecting" | "open" | "unavailable" | "ended"

interface Frame {
  type: "geometry" | "reset" | "end"
  cols?: number
  rows?: number
}

const DEFAULT_COLS = 120
const DEFAULT_ROWS = 40
const MAX_FONT = 13
const MIN_FONT = 6
// A monospace cell is ~0.6em wide; close enough to pick a font that fits.
const CELL_WIDTH_EM = 0.6

export default function SessionConsole({ agent }: { agent: string }) {
  const host = useRef<HTMLDivElement>(null)
  const [status, setStatus] = useState<Status>("connecting")

  useEffect(() => {
    const el = host.current
    if (!el) return
    const term = new Terminal({
      cols: DEFAULT_COLS,
      rows: DEFAULT_ROWS,
      disableStdin: true,
      cursorBlink: false,
      scrollback: 5000,
      fontSize: MAX_FONT,
      fontFamily: 'Menlo, Monaco, "Courier New", monospace',
      theme: { background: "#0b0d10" },
    })
    term.open(el)

    const fitFont = () => {
      const width = el.parentElement?.clientWidth ?? el.clientWidth
      const size = Math.floor(width / (term.cols * CELL_WIDTH_EM))
      term.options.fontSize = Math.max(MIN_FONT, Math.min(MAX_FONT, size))
    }
    fitFont()
    const observer = new ResizeObserver(fitFont)
    if (el.parentElement) observer.observe(el.parentElement)

    const url = new URL(`api/dms/${encodeURIComponent(agent)}/console`, document.baseURI)
    url.protocol = window.location.protocol === "https:" ? "wss:" : "ws:"
    let stopped = false
    let socket: WebSocket | null = null
    let retry: number | null = null
    const connect = () => {
      if (stopped) return
      setStatus("connecting")
      let opened = false
      const next = new WebSocket(url)
      next.binaryType = "arraybuffer"
      socket = next
      next.onmessage = (message) => {
        if (message.data instanceof ArrayBuffer) {
          term.write(new Uint8Array(message.data))
          return
        }
        let frame: Frame
        try {
          frame = JSON.parse(String(message.data)) as Frame
        } catch {
          return
        }
        if (frame.type === "geometry") {
          if (!opened) {
            // Every connection replays from the session's first byte.
            opened = true
            term.reset()
            setStatus("open")
          }
          term.resize(frame.cols || DEFAULT_COLS, frame.rows || DEFAULT_ROWS)
          fitFont()
        } else if (frame.type === "reset") {
          term.reset()
        } else if (frame.type === "end") {
          setStatus("ended")
        }
      }
      next.onclose = () => {
        if (socket !== next || stopped) return
        // Refused before any frame: the seat is not mirrorable (or not live).
        // Either way, keep trying — the operator may be starting it now.
        setStatus((current) => (current === "ended" ? current : opened ? "ended" : "unavailable"))
        retry = window.setTimeout(connect, 3000)
      }
    }
    connect()

    return () => {
      stopped = true
      if (retry !== null) window.clearTimeout(retry)
      socket?.close(1000)
      observer.disconnect()
      term.dispose()
    }
  }, [agent])

  return (
    <div className="flex min-h-0 flex-1 flex-col bg-[#0b0d10]" data-session-console>
      {status !== "open" && (
        <div className="border-b border-white/10 px-3 py-2 text-[11px] text-zinc-300" data-console-status={status}>
          {status === "connecting" && "Connecting to the live session…"}
          {status === "ended" && "The session ended. Waiting for it to come back…"}
          {status === "unavailable" && (
            <>
              No live terminal for <b>{agent}</b>. Only a session launched through bashy can be
              mirrored — start it with <code>bashy chat --agent {agent}</code>.
            </>
          )}
        </div>
      )}
      <div className="min-h-0 flex-1 overflow-auto p-1">
        <div ref={host} />
      </div>
    </div>
  )
}
