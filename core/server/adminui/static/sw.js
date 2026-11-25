/* Company in a Can admin: service worker, Web Push only (specs/025-pwa-webpush).

   It has exactly two listeners, "push" and "notificationclick". It intercepts no requests and keeps
   no cache of any kind, so nothing authenticated is ever stored or served from here: every page and
   API response still comes from the server, as no-store. A page that fails to load
   offline therefore fails as it always did; that is the deliberate trade for not caching private
   data.

   The push payload is generic by design (a title by notification type, a fixed line, a site path
   and a number); the page behind the link shows the details after sign-in. */
"use strict";

self.addEventListener("push", function (event) {
  var d = {};
  try {
    d = event.data ? event.data.json() : {};
  } catch (e) {
    d = {};
  }
  var title = typeof d.t === "string" && d.t ? d.t.slice(0, 80) : "New notification";
  var body = typeof d.b === "string" ? d.b.slice(0, 120) : "";
  var id = typeof d.i === "number" ? d.i : 0;
  // A browser requires every push to show a notification, so one is always shown.
  event.waitUntil(
    self.registration.showNotification(title, {
      body: body,
      icon: "/static/icon-192.png",
      badge: "/static/icon-192.png",
      tag: id ? "n" + id : "can-admin",
      data: { u: sitePath(d.u) },
    })
  );
});

self.addEventListener("notificationclick", function (event) {
  event.notification.close();
  var target = new URL(sitePath(event.notification.data && event.notification.data.u), self.location.origin).href;
  event.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(function (wins) {
      for (var i = 0; i < wins.length; i++) {
        if (new URL(wins[i].url).origin === self.location.origin && "navigate" in wins[i]) {
          return wins[i].navigate(target).then(function (c) {
            return c && c.focus();
          });
        }
      }
      return self.clients.openWindow(target);
    })
  );
});

// sitePath returns u only when it is a path on this site; anything else (another origin, a
// scheme-relative or backslash form, a script URL) becomes the notification list.
function sitePath(u) {
  if (typeof u !== "string" || u.charAt(0) !== "/" || u.charAt(1) === "/" || u.indexOf("\\") >= 0 || u.length > 200) {
    return "/notifications";
  }
  var p = new URL(u, self.location.origin);
  return p.origin === self.location.origin ? p.pathname + p.search : "/notifications";
}
