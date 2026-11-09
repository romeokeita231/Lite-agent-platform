"use strict";

var KEY_STORAGE = "lite-platform-console-key";
var HEALTH_INTERVAL_MS = 5000;
var RUN_POLL_MS = 1000;
var STATUSES = ["queued", "running", "waiting_human", "completed", "failed", "cancelled"];

var gate = document.getElementById("gate");
var checking = document.getElementById("checking");
var shell = document.getElementById("shell");
var form = document.getElementById("gate-form");
var input = document.getElementById("key-input");
var gateError = document.getElementById("gate-error");
var changeKey = document.getElementById("change-key");
var statusFilter = document.getElementById("status-filter");
var agentFilter = document.getElementById("agent-filter");
var runList = document.getElementById("run-list");
var runCounts = document.getElementById("run-counts");
var listError = document.getElementById("list-error");
var listEmpty = document.getElementById("list-empty");

var detailEmpty = document.getElementById("detail-empty");
var detail = document.getElementById("detail");
var detailStatus = document.getElementById("detail-status");
var detailTerminalReason = document.getElementById("detail-terminal-reason");
var detailPendingStep = document.getElementById("detail-pending-step");
var checkpointList = document.getElementById("checkpoint-list");
var traceList = document.getElementById("trace-list");
var detailError = document.getElementById("detail-error");

var active = false;
var listInFlight = false;
var detailInFlight = false;
var selectedRunID = "";
var agentLabels = {};

function agentLabel(id) {
  return agentLabels[id] || id;
}

function formatTime(value) {
  if (!value) {
    return "";
  }
  var date = new Date(value);
  if (isNaN(date.getTime())) {
    return String(value);
  }
  return date.toLocaleString();
}

function formatJSON(value) {
  if (value === null || value === undefined || value === "") {
    return "";
  }
  if (typeof value === "string") {
    return value;
  }
  return JSON.stringify(value, null, 2);
}

function addCell(row, text) {
  var cell = document.createElement("td");
  cell.textContent = text;
  row.appendChild(cell);
}

function getKey() {
  try {
    return window.localStorage.getItem(KEY_STORAGE) || "";
  } catch (e) {
    return "";
  }
}

function setKey(key) {
  try {
    if (key) {
      window.localStorage.setItem(KEY_STORAGE, key);
    } else {
      window.localStorage.removeItem(KEY_STORAGE);
    }
  } catch (e) {
  }
}

function setView(view) {
  gate.hidden = view !== "gate";
  checking.hidden = view !== "checking";
  shell.hidden = view !== "shell";
  changeKey.hidden = view !== "shell";
  active = view === "shell";
  if (active) {
    loadAgents();
    pollList();
  }
}

function showGate(message) {
  gateError.textContent = message || "";
  input.value = "";
  setView("gate");
}

function apiFetch(path, options) {
  var opts = options || {};
  var headers = {};
  Object.keys(opts.headers || {}).forEach(function (name) {
    headers[name] = opts.headers[name];
  });
  var key = getKey();
  if (key) {
    headers.Authorization = "Bearer " + key;
  }
  var request = { headers: headers, cache: "no-store" };
  if (opts.method) {
    request.method = opts.method;
  }
  if (opts.body) {
    request.body = opts.body;
  }
  return fetch(path, request).then(function (response) {
    if (response.status === 401) {
      setKey("");
      showGate("This key was rejected. Paste a valid tenant API key.");
    }
    return response;
  });
}

function verifyKey() {
  return apiFetch("/v1/models").then(function (response) {
    if (response.status === 401) {
      var unauthorized = new Error("unauthorized");
      unauthorized.unauthorized = true;
      throw unauthorized;
    }
    if (!response.ok) {
      throw new Error("key check failed with status " + response.status);
    }
  });
}

function openWithKey(key) {
  setKey(key);
  setView("checking");
  verifyKey().then(function () {
    setView("shell");
  }, function (err) {
    if (err && err.unauthorized) {
      return;
    }
    setKey("");
    showGate("Could not verify the key: " + (err && err.message ? err.message : "request failed"));
  });
}

function renderProbe(node, state, label) {
  node.setAttribute("data-state", state);
  node.querySelector("b").textContent = label;
}

function probeHealth() {
  var nodes = document.querySelectorAll("[data-probe]");
  Array.prototype.forEach.call(nodes, function (node) {
    var path = node.getAttribute("data-probe");
    fetch(path, { cache: "no-store" }).then(function (response) {
      if (response.status === 200) {
        renderProbe(node, "ok", "ok");
      } else if (response.status === 503) {
        renderProbe(node, "bad", "not ready");
      } else {
        renderProbe(node, "bad", "HTTP " + response.status);
      }
    }, function () {
      renderProbe(node, "bad", "unreachable");
    });
  });
}

function listQuery() {
  var params = [];
  if (statusFilter.value) {
    params.push("status=" + encodeURIComponent(statusFilter.value));
  }
  if (agentFilter.value) {
    params.push("agent_id=" + encodeURIComponent(agentFilter.value));
  }
  params.push("limit=50");
  return params.join("&");
}

function pollList() {
  if (!active || listInFlight) {
    return;
  }
  listInFlight = true;
  apiFetch("/v1/runs?" + listQuery()).then(function (response) {
    if (response.status === 401) {
      return null;
    }
    if (!response.ok) {
      throw new Error("run list failed with status " + response.status);
    }
    return response.json();
  }).then(function (body) {
    if (!body) {
      return;
    }
    listError.textContent = "";
    renderCounts(body.counts || {});
    renderRuns(body.runs || []);
  }).catch(function (err) {
    listError.textContent = err && err.message ? err.message : "run list request failed";
  }).then(function () {
    listInFlight = false;
  });
}

function renderCounts(counts) {
  runCounts.textContent = "";
  STATUSES.forEach(function (status) {
    var pill = document.createElement("span");
    pill.className = "count";
    pill.setAttribute("data-status", status);
    var name = document.createElement("span");
    name.textContent = status;
    var value = document.createElement("b");
    value.textContent = String(counts[status] || 0);
    pill.appendChild(name);
    pill.appendChild(value);
    runCounts.appendChild(pill);
  });
}

function renderRuns(runs) {
  runList.textContent = "";
  listEmpty.hidden = runs.length > 0;
  runs.forEach(function (run) {
    var row = document.createElement("tr");
    row.setAttribute("data-run-id", run.id);
    if (run.id === selectedRunID) {
      row.className = "selected";
    }
    row.addEventListener("click", function () {
      selectRun(run.id);
    });
    addCell(row, formatTime(run.created_at));
    addCell(row, agentLabel(run.agent_id));
    var statusCell = document.createElement("td");
    var badge = document.createElement("span");
    badge.className = "status status-" + run.status;
    badge.textContent = run.status;
    statusCell.appendChild(badge);
    row.appendChild(statusCell);
    addCell(row, String(run.attempt || 0));
    runList.appendChild(row);
  });
}

function loadAgents() {
  apiFetch("/v1/agents").then(function (response) {
    if (response.status === 401) {
      return null;
    }
    if (!response.ok) {
      throw new Error("agent list failed with status " + response.status);
    }
    return response.json();
  }).then(function (agents) {
    if (!agents) {
      return;
    }
    var current = agentFilter.value;
    agentLabels = {};
    while (agentFilter.options.length > 1) {
      agentFilter.remove(1);
    }
    agents.forEach(function (agent) {
      agentLabels[agent.id] = agent.name || agent.id;
      var option = document.createElement("option");
      option.value = agent.id;
      option.textContent = agent.name || agent.id;
      agentFilter.appendChild(option);
    });
    agentFilter.value = current;
    pollList();
  }).catch(function () {
    return null;
  });
}

function selectRun(id) {
  var same = selectedRunID === id;
  selectedRunID = id;
  detailEmpty.hidden = true;
  detailEmpty.textContent = "Select a run to inspect it.";
  detail.hidden = false;
  detailError.textContent = "";
  if (!same) {
    resetDetail();
  }
  pollDetail();
}

function resetDetail() {
  detailStatus.textContent = "";
  detailStatus.className = "status";
  detailTerminalReason.textContent = "";
  detailPendingStep.textContent = "";
  checkpointList.textContent = "";
  traceList.textContent = "";
}

function fetchDetailJSON(path) {
  return apiFetch(path).then(function (response) {
    if (response.status !== 200) {
      return { status: response.status };
    }
    return response.json().then(function (body) {
      return { status: 200, body: body };
    }, function () {
      return { status: 200, body: null };
    });
  });
}

function pollDetail() {
  if (!active || detailInFlight || !selectedRunID) {
    return;
  }
  var id = selectedRunID;
  detailInFlight = true;
  Promise.all([
    fetchDetailJSON("/v1/runs/" + encodeURIComponent(id)),
    fetchDetailJSON("/v1/runs/" + encodeURIComponent(id) + "/steps"),
    fetchDetailJSON("/v1/runs/" + encodeURIComponent(id) + "/trace")
  ]).then(function (results) {
    if (id !== selectedRunID) {
      return;
    }
    var run = results[0];
    if (run.status === 401) {
      return;
    }
    if (run.status === 404) {
      detail.hidden = true;
      detailEmpty.hidden = false;
      detailEmpty.textContent = "run not found";
      detailError.textContent = "";
      return;
    }
    if (run.status !== 200 || !run.body) {
      detailError.textContent = "run detail failed with status " + run.status;
      return;
    }
    detailError.textContent = "";
    renderFacts(run.body);
    renderCheckpoints(results[1].status === 200 && results[1].body ? results[1].body : []);
    renderTrace(results[2].status === 200 && results[2].body ? results[2].body : null);
  }).catch(function (err) {
    detailError.textContent = err && err.message ? err.message : "run detail request failed";
  }).then(function () {
    detailInFlight = false;
  });
}

function renderFacts(run) {
  detailStatus.textContent = run.status || "";
  detailStatus.className = "status status-" + (run.status || "");
  detailTerminalReason.textContent = run.terminal_reason ? String(run.terminal_reason) : "—";
  detailPendingStep.textContent = formatJSON(run.pending_step);
}

function renderCheckpoints(steps) {
  checkpointList.textContent = "";
  if (!steps.length) {
    var empty = document.createElement("li");
    empty.className = "placeholder";
    empty.textContent = "No checkpoints yet.";
    checkpointList.appendChild(empty);
    return;
  }
  steps.forEach(function (step) {
    var item = document.createElement("li");
    var head = document.createElement("div");
    head.className = "checkpoint-head";
    head.textContent = "Step " + step.step_index + " · " + step.step_type + " · " + formatTime(step.created_at);
    item.appendChild(head);
    var body = document.createElement("pre");
    body.textContent = formatJSON(step.result);
    item.appendChild(body);
    checkpointList.appendChild(item);
  });
}

function renderTrace(trace) {
  traceList.textContent = "";
  var steps = trace && trace.steps ? trace.steps : [];
  if (!steps.length) {
    var empty = document.createElement("p");
    empty.className = "placeholder";
    empty.textContent = "No spans yet.";
    traceList.appendChild(empty);
    return;
  }
  steps.forEach(function (step) {
    var group = document.createElement("details");
    group.className = "trace-step";
    group.open = true;
    var summary = document.createElement("summary");
    summary.textContent = "Step " + step.step_index + " · " + step.spans.length + " span(s)";
    group.appendChild(summary);
    step.spans.forEach(function (span) {
      var row = document.createElement("div");
      row.className = span.error ? "span span-error" : "span";
      var title = document.createElement("div");
      title.className = "span-title";
      var name = document.createElement("b");
      name.textContent = span.name;
      title.appendChild(name);
      var attempt = document.createElement("span");
      attempt.className = "attempt";
      attempt.textContent = "attempt " + span.attempt;
      title.appendChild(attempt);
      row.appendChild(title);
      var meta = document.createElement("div");
      meta.className = "span-meta";
      meta.textContent = formatTime(span.started_at) + " → " + formatTime(span.ended_at);
      row.appendChild(meta);
      if (span.error) {
        var errorLine = document.createElement("div");
        errorLine.className = "span-meta";
        errorLine.textContent = span.error;
        row.appendChild(errorLine);
      }
      var attributes = document.createElement("pre");
      attributes.textContent = formatJSON(span.attributes);
      row.appendChild(attributes);
      group.appendChild(row);
    });
    traceList.appendChild(group);
  });
}

statusFilter.addEventListener("change", pollList);
agentFilter.addEventListener("change", pollList);

form.addEventListener("submit", function (event) {
  event.preventDefault();
  var key = input.value.trim();
  if (!key) {
    gateError.textContent = "Enter a tenant API key.";
    return;
  }
  openWithKey(key);
});

changeKey.addEventListener("click", function () {
  setKey("");
  showGate("Enter another tenant API key.");
});

if (getKey()) {
  openWithKey(getKey());
} else {
  showGate("");
}
probeHealth();
window.setInterval(probeHealth, HEALTH_INTERVAL_MS);
window.setInterval(pollList, RUN_POLL_MS);
window.setInterval(pollDetail, RUN_POLL_MS);
