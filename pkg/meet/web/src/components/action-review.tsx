import { useEffect, useState } from "react"
import { z } from "zod"
import { Button } from "@/components/ui/button"

const actionSchema = z.object({ id: z.string(), text: z.string(), owner: z.string().optional(), source: z.string(), adopted: z.boolean(), story: z.string().optional() })
const listSchema = z.object({ actions: actionSchema.array().nullable() })
const resultSchema = z.object({ room: z.string(), preview: z.boolean(), results: z.array(z.object({ story: z.string().optional(), note: z.string().optional(), action: z.object({ id: z.string(), text: z.string() }).passthrough() }).passthrough()).optional() })
type Action = z.infer<typeof actionSchema>
type Terms = { actions: { action: string; owner: string; accept: string[]; depends: string[] }[]; sprint: number }
async function api(room: string, suffix = "", body?: unknown) {
  const response = await fetch(new URL(`api/rooms/${encodeURIComponent(room)}/actions${suffix}`, document.baseURI), body === undefined ? {} : { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) })
  const data = await response.json()
  if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`)
  return data
}

// This is a client of the existing adoption API, never an authority source.
// Confirmation submits exactly the terms displayed by the last successful preview.
export function ActionReview({ room }: { room: string }) {
  const [actions, setActions] = useState<Action[]>([])
  const [selected, setSelected] = useState("")
  const [owner, setOwner] = useState("")
  const [accept, setAccept] = useState("")
  const [depends, setDepends] = useState("")
  const [sprint, setSprint] = useState("")
  const [preview, setPreview] = useState<{ terms: Terms; text: string; source: string; artifact: z.infer<typeof resultSchema> } | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const [result, setResult] = useState("")
  useEffect(() => {
    let active = true
    setPreview(null); setSelected(""); setResult(""); setActions([])
    api(room).then(data => { if (active) { setActions(listSchema.parse(data).actions || []); setError("") } }).catch(e => { if (active) setError(String(e.message)) })
    return () => { active = false }
  }, [room])
  const invalidate = () => { setPreview(null); setResult("") }
  const action = actions.find(a => a.id === selected)
  async function run(confirm: boolean) {
    if (!action || busy) return
    setBusy(true); setError("")
    try {
      if (confirm && !preview) return
      const terms: Terms = confirm ? preview!.terms : { actions: [{ action: selected, owner: owner.trim(), accept: accept.split("\n").map(s => s.trim()).filter(Boolean), depends: depends.split(",").map(s => s.trim()).filter(Boolean) }], sprint: Number(sprint) }
      if (!Number.isSafeInteger(terms.sprint) || terms.sprint <= 0) throw new Error("Choose a positive sprint number.")
      if (!confirm && terms.actions[0].accept.length === 0) throw new Error("Provide acceptance criteria before review.")
      // Refresh the concrete candidate before confirmation. A retracted or changed
      // candidate invalidates the displayed preview rather than adopting stale prose.
      if (confirm) {
        const fresh = listSchema.parse(await api(room)).actions || []
        const current = fresh.find(a => a.id === selected)
        setActions(fresh)
        if (!current || current.text !== preview!.text || current.source !== preview!.source || current.adopted) { setPreview(null); throw new Error("The action changed or was adopted. Review a fresh preview.") }
      }
      const response = resultSchema.parse(await api(room, "/adopt", { ...terms, confirm }))
      if (confirm) {
        setPreview(null)
        setResult((response.results || []).map(r => `${r.story || ""} ${r.note || ""}`.trim()).join("\n") || "Adoption recorded.")
        setActions(listSchema.parse(await api(room)).actions || [])
      } else {
        if (!response.preview) throw new Error("Expected a read-only preview.")
        if (!response.results?.length || response.results[0].action.id !== selected) throw new Error("Preview did not return the selected action artifact.")
        setPreview({ terms, text: action.text, source: action.source, artifact: response })
      }
    } catch (e) { setError(e instanceof Error ? e.message : String(e)) }
    finally { setBusy(false) }
  }
  const input = "w-full rounded border p-2 text-xs"
  return <section aria-label="Review room actions" className="space-y-3">
    <h2 className="text-sm font-semibold">Actions awaiting review</h2>
    <p className="text-xs">Review a concrete action before creating tracked work. Adoption does not approve execution or accept a completed milestone.</p>
    {actions.filter(a => a.adopted).map(a => <p className="text-xs" key={a.id}>Tracked: {a.text} · {a.story}</p>)}
    <label className="block text-xs">Candidate action<select className={input} value={selected} disabled={busy} onChange={e => { invalidate(); setSelected(e.target.value); setOwner(actions.find(a => a.id === e.target.value)?.owner || "") }}><option value="">Select an action</option>{actions.filter(a => !a.adopted).map(a => <option key={a.id} value={a.id}>{a.source}: {a.text}</option>)}</select></label>
    {action && <>
      <p className="text-xs">Source: {action.source} · {action.id}</p>
      <label className="block text-xs">Sprint<input className={input} value={sprint} disabled={busy} onChange={e => { invalidate(); setSprint(e.target.value) }} /></label>
      <label className="block text-xs">Owner<input className={input} value={owner} disabled={busy} onChange={e => { invalidate(); setOwner(e.target.value) }} /></label>
      <label className="block text-xs">Acceptance criteria<textarea className={input} value={accept} disabled={busy} onChange={e => { invalidate(); setAccept(e.target.value) }} /></label>
      <label className="block text-xs">Dependencies (comma separated)<input className={input} value={depends} disabled={busy} onChange={e => { invalidate(); setDepends(e.target.value) }} /></label>
      <Button disabled={busy} onClick={() => void run(false)}>Preview tracked work</Button>
    </>}
    {preview && <div className="space-y-2 rounded border p-2" aria-label="Adoption preview">
      <p className="text-xs">Decision: create a story for this exact action and these terms?</p>
      <p className="text-xs">Consequences: adds linked work to sprint {preview.terms.sprint}. The owner must deliver evidence; milestone review and execution approvals remain separate.</p>
      <p className="text-xs">Artifact: room {room}, action {selected} ({preview.source})</p>
      <p className="text-xs">{preview.text}</p><pre className="whitespace-pre-wrap text-xs">{JSON.stringify(preview.terms, null, 2)}</pre>
      <p className="text-xs">Server preview artifact:</p>
      <pre aria-label="Server preview artifact" className="whitespace-pre-wrap text-xs">{JSON.stringify(preview.artifact, null, 2)}</pre>
      <Button disabled={busy} onClick={() => void run(true)}>Confirm adoption</Button>
    </div>}
    {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    {result && <p role="status" className="text-xs">{result}</p>}
  </section>
}
