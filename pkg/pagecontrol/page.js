(function (scope) {
  "use strict";
  if (scope.BashyPage) return;
  const handlers = new Map();
  let transport = null;
  let epoch = 0;
  const describe = () => Array.from(handlers, ([name, action]) => ({ name, description: action.description || "" }));
  function advertise() { if (transport && transport.socket && transport.socket.readyState === 1) transport.socket.send(JSON.stringify({ actions: describe(), label: transport.label })); }
  function register(name, action) {
    if (typeof name !== "string" || !/^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$/.test(name) || !action || typeof action.run !== "function") throw new Error("Invalid page action");
    if (handlers.has(name)) throw new Error("Page action already registered: " + name);
    if (action.description !== undefined && typeof action.description !== "string") throw new Error("Invalid page action description");
    if (new TextEncoder().encode(action.description || "").length > 512) throw new Error("Page action description too long");
    handlers.set(name, action); advertise();
    return () => { if (handlers.get(name) === action) { handlers.delete(name); advertise(); } };
  }
  async function invoke(name, input) {
    const action = handlers.get(name);
    if (!action) throw new Error("Page action is not mounted: " + name);
    return await action.run(input);
  }
  function disconnect() {
    epoch++;
    const old = transport; transport = null;
    if (old) { clearTimeout(old.retry); clearInterval(old.heartbeat); if (old.socket) old.socket.close(); }
  }
  function connect(options) {
    disconnect();
    if (!options || !options.url) throw new Error("Page action websocket URL required");
    const url = new URL(options.url, scope.document ? scope.document.baseURI : scope.location.href);
    if (url.protocol === "https:") url.protocol = "wss:";
    if (url.protocol === "http:") url.protocol = "ws:";
    if (url.protocol !== "ws:" && url.protocol !== "wss:") throw new Error("Invalid page action websocket URL");
    // A page must never send action results or form data to another origin.
    if (url.host !== scope.location.host || (url.protocol === "wss:") !== (scope.location.protocol === "https:")) throw new Error("Page action websocket must be same-origin");
    let label = "";
    for (const char of String(options.label || "")) { if (new TextEncoder().encode(label + char).length > 160) break; label += char; }
    const current = { socket: null, retry: null, heartbeat: null, label };
    transport = current;
    const generation = epoch;
    function open() {
      if (generation !== epoch) return;
      const ws = new scope.WebSocket(url.href); current.socket = ws;
      const received = new Set();
      ws.onopen = () => { if (generation !== epoch || ws !== current.socket) { ws.close(); return; } advertise(); current.heartbeat = setInterval(advertise, 15000); };
      ws.onmessage = event => {
        if (generation !== epoch || ws !== current.socket) return;
        let message;
        try { message = JSON.parse(event.data); } catch (_) { ws.close(); return; }
        const command = message.command;
        if (!command) return;
        if (typeof command.id !== "string" || typeof command.action !== "string" || received.has(command.id)) return;
        if (received.size >= 128) { ws.close(); return; }
        received.add(command.id);
        const send = value => {
          if (generation !== epoch || ws !== current.socket || ws.readyState !== 1) return;
          try {
            const result = JSON.stringify(value);
            if (new TextEncoder().encode(result).length > 32768) ws.send(JSON.stringify({ id: command.id, error: "Action result too large" }));
            else ws.send(result);
          } catch (_) { ws.send(JSON.stringify({ id: command.id, error: "Action result is not JSON serializable" })); }
        };
        Promise.resolve().then(() => {
          if (generation !== epoch || ws !== current.socket || ws.readyState !== 1) throw new Error("Page disconnected before action began");
          return invoke(command.action, command.input);
        }).then(result => send({ id: command.id, result: result === undefined ? null : result }), error => send({ id: command.id, error: String(error && error.message || error).slice(0, 500) }));
      };
      ws.onclose = () => { if (generation !== epoch || ws !== current.socket) return; clearInterval(current.heartbeat); current.socket = null; current.retry = setTimeout(open, 2000); };
      ws.onerror = () => { ws.close(); };
    }
    open();
    return () => { if (generation === epoch) disconnect(); };
  }
  scope.BashyPage = Object.freeze({ register, describe, invoke, connect, disconnect });
})(typeof window !== "undefined" ? window : globalThis);
