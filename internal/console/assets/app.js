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

var active = false;
var listInFlight = false;
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
