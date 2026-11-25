/* Company in a Can admin: enable or disable Web Push on this device (specs/025-pwa-webpush).

   Progressive enhancement for /notifications only. Without JavaScript, or in a browser without Web
   Push, the page still lists the devices, removes them and sends a test notification; this file only
   adds the "enable on this device" step that has to run in the browser. No inline script, no eval,
   no HTML built from strings. */
(function () {
  "use strict";
  var root = document.getElementById("push");
  if (!root || !("serviceWorker" in navigator) || !("PushManager" in window) || !("Notification" in window)) {
    return;
  }
  var enable = document.getElementById("push-enable");
  var disable = document.getElementById("push-disable");
  var status = document.getElementById("push-status");
  var csrf = root.getAttribute("data-csrf");
  var keyURL = root.getAttribute("data-key-url");
  var subURL = root.getAttribute("data-sub-url");
  var msg = {
    denied: root.getAttribute("data-msg-denied"),
    failed: root.getAttribute("data-msg-failed"),
    on: root.getAttribute("data-msg-on"),
  };

  function say(text) {
    status.textContent = text || "";
    status.hidden = !text;
  }

  function api(method, url, body) {
    var init = { method: method, credentials: "same-origin", headers: { "X-CSRF-Token": csrf } };
    if (body !== undefined) {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
    return window.fetch(url, init).then(function (r) {
      if (!r.ok) {
        throw new Error("HTTP " + r.status);
      }
      return r;
    });
  }

  function keyBytes(b64url) {
    var pad = "=".repeat((4 - (b64url.length % 4)) % 4);
    var raw = atob((b64url + pad).replace(/-/g, "+").replace(/_/g, "/"));
    var out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) {
      out[i] = raw.charCodeAt(i);
    }
    return out;
  }

  function current() {
    return navigator.serviceWorker.getRegistration("/").then(function (reg) {
      return reg ? reg.pushManager.getSubscription() : null;
    });
  }

  function show(sub) {
    enable.hidden = !!sub;
    disable.hidden = !sub;
    if (sub) {
      say(msg.on);
    }
  }

  enable.addEventListener("click", function () {
    enable.disabled = true;
    say("");
    Notification.requestPermission()
      .then(function (perm) {
        if (perm !== "granted") {
          throw new Error("denied");
        }
        return navigator.serviceWorker.register("/sw.js", { scope: "/" });
      })
      .then(function () {
        return navigator.serviceWorker.ready;
      })
      .then(function (reg) {
        return api("GET", keyURL)
          .then(function (r) {
            return r.json();
          })
          .then(function (k) {
            return reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(k.public_key) });
          });
      })
      .then(function (sub) {
        return api("POST", subURL, sub.toJSON());
      })
      .then(function () {
        window.location.reload();
      })
      .catch(function (e) {
        enable.disabled = false;
        say(e && e.message === "denied" ? msg.denied : msg.failed);
      });
  });

  disable.addEventListener("click", function () {
    disable.disabled = true;
    current()
      .then(function (sub) {
        if (!sub) {
          return null;
        }
        return api("DELETE", subURL, { endpoint: sub.endpoint }).catch(function () {}).then(function () {
          return sub.unsubscribe();
        });
      })
      .then(function () {
        window.location.reload();
      })
      .catch(function () {
        disable.disabled = false;
        say(msg.failed);
      });
  });

  current().then(show).catch(function () {
    show(null);
  });
})();
