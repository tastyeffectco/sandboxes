package wake

import (
	"io"
	"net/http"
	"strconv"
	"strings"
)

// The wake pages are white-label: no sandbox ids, no internal reason
// codes, no host jargon in the body. The machine-readable reason still
// travels in response headers (X-Wake-Error / X-Retry-After-Reason)
// for log correlation. CSS `%` literals are kept out of fmt — the
// templates use placeholder tokens and strings.NewReplacer.

// writeRefreshPage emits the "spinning up your app" page with a
// meta-refresh. By the time the browser refreshes, Traefik's Docker
// provider has typically observed the `docker start` and the dynamic
// per-host route (priority 100) wins over the catch-all (priority 1),
// so the second request hits the live container directly.
func writeRefreshPage(w http.ResponseWriter, status int, id string, refreshSec int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body := strings.NewReplacer("@REFRESH@", strconv.Itoa(refreshSec)).Replace(refreshTmpl)
	_, _ = io.WriteString(w, body)
}

// writeBusyPage is the 503-shape page used when admission denies a
// wake. The Retry-After contract is unchanged; the body is white-label
// and self-refreshes after the retry window. `reason` travels only in
// the header.
func writeBusyPage(w http.ResponseWriter, id, reason string, retryAfter int, availPct float64) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.Header().Set("X-Retry-After-Reason", reason)
	w.WriteHeader(http.StatusServiceUnavailable)
	body := strings.NewReplacer("@RETRY@", strconv.Itoa(retryAfter)).Replace(busyTmpl)
	_, _ = io.WriteString(w, body)
}

// writeErrorPage is the 503 page used when wake failed for a non-
// admission reason (start failed, tcp-ready timeout, not_found, …).
// The body is white-label; X-Wake-Error carries the precise reason.
func writeErrorPage(w http.ResponseWriter, id, reason string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Wake-Error", reason)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, errorTmpl)
}

// --- shared style -----------------------------------------------------
// Kept as one const so the three pages stay a visual family.
const pageCSS = `
  *{box-sizing:border-box}
  html,body{height:100%}
  body{margin:0;font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;
       display:flex;align-items:center;justify-content:center;
       background:linear-gradient(160deg,#f6f7fb 0%,#eceef6 100%);color:#1c1c2e}
  .card{width:90%;max-width:25rem;background:#fff;border-radius:20px;
        padding:2.75rem 2rem;text-align:center;
        box-shadow:0 12px 44px rgba(28,28,70,.12)}
  h1{font-size:1.28rem;margin:1.1rem 0 .4rem;font-weight:650}
  p{margin:0;color:#6c6c86;font-size:.95rem;line-height:1.5}
  .emoji{font-size:3.5rem;line-height:1;display:inline-block}
  .bar{margin:1.7rem auto 0;width:78%;height:8px;border-radius:99px;
       background:#ebebf3;overflow:hidden;position:relative}
  .bar::after{content:"";position:absolute;top:0;height:100%;width:42%;
       border-radius:99px;background:linear-gradient(90deg,#6c8cff,#a06bff);
       animation:shimmer 1.25s ease-in-out infinite}
  @keyframes shimmer{0%{left:-45%}100%{left:103%}}
  button{margin-top:1.5rem;font:inherit;font-size:.95rem;font-weight:600;color:#fff;
       cursor:pointer;border:0;border-radius:11px;padding:.7rem 1.7rem;
       background:linear-gradient(90deg,#6c8cff,#a06bff)}
  button:active{transform:translateY(1px)}
  @media(prefers-reduced-motion:reduce){
    .emoji,.bar::after{animation:none!important}.bar::after{left:30%}}
`

// --- the three pages --------------------------------------------------

const refreshTmpl = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Spinning up your app…</title>
<meta http-equiv="refresh" content="@REFRESH@">
<style>` + pageCSS + `
  .emoji{animation:float 2.2s ease-in-out infinite}
  @keyframes float{0%,100%{transform:translateY(0) rotate(-6deg)}
                   50%{transform:translateY(-12px) rotate(6deg)}}
</style>
</head>
<body>
  <div class="card">
    <div class="emoji">🚀</div>
    <h1>Spinning up your app!</h1>
    <p>Almost there…</p>
    <div class="bar"></div>
  </div>
</body>
</html>
`

const busyTmpl = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Almost ready…</title>
<meta http-equiv="refresh" content="@RETRY@">
<style>` + pageCSS + `
  .emoji{animation:tilt 2s ease-in-out infinite}
  @keyframes tilt{0%,100%{transform:rotate(-9deg)}50%{transform:rotate(9deg)}}
</style>
</head>
<body>
  <div class="card">
    <div class="emoji">⏳</div>
    <h1>Almost ready…</h1>
    <p>A lot is happening right now — your app will be back in a few seconds.</p>
    <div class="bar"></div>
  </div>
</body>
</html>
`

const errorTmpl = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>We couldn't load your app</title>
<style>` + pageCSS + `</style>
</head>
<body>
  <div class="card">
    <div class="emoji">🛰️</div>
    <h1>We couldn't load your app</h1>
    <p>Something hiccuped while starting it up.<br>Give it another try in a moment.</p>
    <button onclick="location.reload()">Try again</button>
  </div>
</body>
</html>
`
