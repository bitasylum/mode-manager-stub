"use strict";
// The page polls api/state, and posts changes to api/mode and api/move. All
// URLs are relative, so it works through kubectl port-forward and through
// the API server's service proxy alike.

const $ = (id) => document.getElementById(id);

function text(el, value) {
  el.textContent = value == null ? "" : String(value);
}

function fill(dl, rows) {
  dl.replaceChildren();
  for (const [k, v] of rows) {
    const dt = document.createElement("dt");
    const dd = document.createElement("dd");
    text(dt, k);
    text(dd, v);
    dl.append(dt, dd);
  }
}

// options fills a form's select with the modes, and disables the form while
// there are none, such as before the CRDs are installed.
function options(form, select, modes, current) {
  const keep = select.value;
  select.replaceChildren();
  for (const m of modes) {
    const o = document.createElement("option");
    o.value = m;
    text(o, m);
    select.append(o);
  }
  select.value = modes.includes(keep) ? keep : current;
  for (const el of form.elements) {
    el.disabled = modes.length === 0;
  }
}

async function send(method, path, body) {
  try {
    const res = await fetch(path, {
      method: method,
      headers: { "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const out = await res.json();
    text($("result"), out.ok ? "Done." : (out.reason ? out.reason + ": " : "") + out.message);
  } catch (e) {
    text($("result"), "Request failed: " + e);
  }
  refresh();
}

function post(path, body) {
  return send("POST", path, body);
}

function render(s) {
  text($("mode"), s.mode || "none");
  text($("phase"), s.phase);
  $("error").hidden = !s.lastError;
  text($("error"), s.lastError);

  const modes = s.modes || [];
  const allowed = s.next || [];
  const next = $("next");
  next.replaceChildren();
  if (allowed.length) {
    const label = document.createElement("span");
    label.className = "note";
    text(label, s.restricted ? "Allowed next: " : "Any mode: ");
    next.append(label);
  }
  for (const m of allowed) {
    const b = document.createElement("button");
    text(b, m);
    b.addEventListener("click", () => post("api/mode", { mode: m, check: true }));
    next.append(b);
  }
  options($("set"), $("set-mode"), modes, s.mode);
  options($("move"), $("move-target"), modes, s.mode);

  const cm = s.clusterMode;
  const status = cm ? [
    ["Mode", cm.mode],
    ["Set by", cm.modeOwner],
    ["Settled", cm.settled ? "yes" : "no"],
    ["Applied", cm.applied],
    ["Ready", cm.ready],
    ["Degraded", cm.degraded],
    ["Workloads", `${cm.workloads.settled} of ${cm.workloads.total} settled`],
    ["Replicas", `${cm.replicas.ready} ready of ${cm.replicas.desired} desired`],
  ] : [["ClusterMode", "not written yet"]];
  if (s.rulesError) {
    status.push(["Rules", s.rulesError]);
  }
  fill($("status"), status);
  const unsettled = $("unsettled");
  const rows = unsettled.querySelector("tbody");
  rows.replaceChildren();
  for (const w of (cm && cm.unsettled) || []) {
    const tr = document.createElement("tr");
    for (const v of [`${w.kind} ${w.namespace}/${w.name}`, w.state, w.message]) {
      const td = document.createElement("td");
      text(td, v);
      tr.append(td);
    }
    rows.append(tr);
  }
  unsettled.hidden = rows.children.length === 0;

  const steps = $("steps");
  steps.replaceChildren();
  if (s.move) {
    for (const st of s.move.steps || []) {
      const li = document.createElement("li");
      text(li, `${st.from} → ${st.to}: ${st.phase}`);
      steps.append(li);
    }
    const li = document.createElement("li");
    text(li, s.move.done ? (s.move.error ? "Stopped: " + s.move.error : "Reached " + s.move.target) : "Moving to " + s.move.target + "…");
    steps.append(li);
  }
  $("stop-move").hidden = !s.move || s.move.done;

  const st = s.startup;
  fill($("startup"), st ? [
    ["Decision", st.case],
    ["Mode", st.mode],
    ["Restored", st.restored ? "yes" : "no"],
    ["Event", st.event ? `${st.event}: ${st.message}` : ""],
    ["Adopted", s.lastAdoption ? `${s.lastAdoption.mode} from ${s.lastAdoption.by}` : ""],
  ] : [["Decision", "not made yet"]]);
  const fp = $("fingerprint").querySelector("tbody");
  fp.replaceChildren();
  const ids = s.fingerprint || {};
  for (const node of Object.keys(ids).sort()) {
    const tr = document.createElement("tr");
    for (const v of [node, ids[node]]) {
      const td = document.createElement("td");
      text(td, v);
      tr.append(td);
    }
    fp.append(tr);
  }
}

async function refresh() {
  try {
    const res = await fetch("api/state");
    render(await res.json());
  } catch (e) {
    text($("phase"), "unreachable");
  }
}

$("set").addEventListener("submit", (e) => {
  e.preventDefault();
  post("api/mode", { mode: $("set-mode").value, check: $("set-check").checked });
});
$("move").addEventListener("submit", (e) => {
  e.preventDefault();
  post("api/move", { target: $("move-target").value });
});
$("stop-move").addEventListener("click", () => send("DELETE", "api/move"));
$("raw").addEventListener("submit", (e) => {
  e.preventDefault();
  post("api/mode", { mode: $("raw-mode").value.trim(), check: false });
});

refresh();
setInterval(refresh, 2000);
