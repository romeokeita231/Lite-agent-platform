"use strict";

var KEY_STORAGE = "lite-platform-console-key";
var HEALTH_INTERVAL_MS = 5000;

var gate = document.getElementById("gate");
var checking = document.getElementById("checking");
var shell = document.getElementById("shell");
var form = document.getElementById("gate-form");
var input = document.getElementById("key-input");
var gateError = document.getElementById("gate-error");
var changeKey = document.getElementById("change-key");

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
