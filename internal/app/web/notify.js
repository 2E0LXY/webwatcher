// Optional: keeps the unread badge current. Same-origin, no libraries.
(function () {
  var badge = document.getElementById("unread");
  if (!badge || !window.fetch) return;
  function poll() {
    fetch("/notifications/count", {credentials: "same-origin", cache: "no-store"})
      .then(function (r) { return r.ok ? r.text() : ""; })
      .then(function (t) {
        var n = parseInt(t, 10);
        if (isNaN(n)) return;
        badge.textContent = n;
        badge.hidden = n === 0;
      })
      .catch(function () {});
  }
  setInterval(poll, 60000);
})();
