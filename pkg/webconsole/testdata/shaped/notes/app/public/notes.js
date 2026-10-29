// Relative URLs only: the app is served under a mount (and under a preview
// prefix), never at the site root.
async function load() {
  const res = await fetch("api/list");
  const notes = res.ok ? await res.json() : [];
  const ul = document.getElementById("notes");
  ul.replaceChildren(...notes.map((n) => {
    const li = document.createElement("li");
    li.textContent = n;
    return li;
  }));
}
document.getElementById("add").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const input = document.getElementById("note");
  if (!input.value.trim()) return;
  await fetch("api/add", { method: "POST", headers: { "Content-Type": "text/plain" }, body: input.value });
  input.value = "";
  load();
});
load();
