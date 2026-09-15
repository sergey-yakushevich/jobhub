package httpapi

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

// dayBarView is one column of the chart: a day, or a week on a long-lived
// board. Pct is the height as a share of the busiest column, so the chart
// always fills its box whatever the traffic. Title is the hover text.
type dayBarView struct {
	Day   string
	Title string
	Pct   int
}

// statRowView is one line of a breakdown table: what it is, how often, and an
// optional note.
type statRowView struct {
	Label string
	Count int64
	Note  string
}

func plainRows(rows []store.LabelStat) []statRowView {
	out := make([]statRowView, len(rows))
	for i, r := range rows {
		out[i] = statRowView{Label: r.Label, Count: r.Count}
	}
	return out
}

var pageFuncs = template.FuncMap{
	"ts":     func(t time.Time) string { return t.UTC().Format("Jan 2, 2006 15:04:05") },
	"dur":    humanDuration,
	"inputs": inputKinds,
	"ms": func(ms int64) string {
		if ms <= 0 {
			return ""
		}
		return humanDuration(time.Duration(ms) * time.Millisecond)
	},
	"secs": func(sec int64) string {
		if sec <= 0 {
			return ""
		}
		return humanDuration(time.Duration(sec) * time.Second)
	},
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// inputKinds renders the input bitmask the tracker reports. This is the honest
// version of "is it a person": it says what the visitor actually did.
func inputKinds(flags int64) string {
	var kinds []string
	for _, f := range []struct {
		bit  int64
		name string
	}{
		{1, "mouse"}, {2, "scroll"}, {4, "touch"}, {8, "keys"}, {16, "clicks"},
	} {
		if flags&f.bit != 0 {
			kinds = append(kinds, f.name)
		}
	}
	if len(kinds) == 0 {
		return "no input"
	}
	return strings.Join(kinds, " · ")
}

func networkEmoji(net string) string {
	switch strings.ToLower(net) {
	case "youtube":
		return "▶️"
	case "instagram":
		return "📸"
	case "tiktok":
		return "🎵"
	case "x", "twitter":
		return "⚫"
	case "linkedin":
		return "💼"
	case "reddit":
		return "👽"
	default:
		return "🔗"
	}
}

// pageHead is the head every page shares. The icon hrefs carry BasePath
// because the pages sit at several depths under the /trk mount, so a relative
// href would resolve to a different place on each of them. Every template's
// data therefore carries Base.
//
// The inline script runs before the stylesheet so the saved theme lands on
// <html> before first paint — without it a light-theme reader gets a dark
// flash on every navigation.
const pageHead = `<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<link rel="icon" type="image/svg+xml" href="{{.Base}}/favicon.svg">
<link rel="icon" type="image/png" sizes="32x32" href="{{.Base}}/favicon.png">
<link rel="apple-touch-icon" href="{{.Base}}/apple-touch-icon.png">
<script>(function(){var t='dark';try{t=localStorage.getItem('jobhub-theme')||t}catch(e){}document.documentElement.dataset.theme=t})()</script>`

// themeSeg is the Light/Dark segmented control. The active face is pure CSS
// keyed off <html data-theme>, so the buttons never need re-rendering.
const themeSeg = `<div class="seg"><button type="button" class="t-light" onclick="jhTheme('light')">Light</button><button type="button" class="t-dark" onclick="jhTheme('dark')">Dark</button></div>`

const themeJS = `<script>function jhTheme(t){document.documentElement.dataset.theme=t;try{localStorage.setItem('jobhub-theme',t)}catch(e){}}</script>`

// chartCSS styles the day/week column chart and the tooltip that follows the
// cursor across it.
const chartCSS = `
  .panel { padding:14px 16px; margin-top:12px; }
  .chart { display:flex; align-items:flex-end; gap:3px; height:72px; }
  .chart .col { flex:1 1 0; min-width:2px; height:100%; display:flex; align-items:flex-end; }
  .chart .bar { width:100%; background:color-mix(in srgb, var(--accent) 70%, transparent); border-radius:3px 3px 0 0; }
  .chart .col:hover .bar { background:var(--accent); }
  .axis { display:flex; justify-content:space-between; color:var(--hint); font-size:11px; margin-top:6px; }
  #tip { position:fixed; top:0; left:0; z-index:9; pointer-events:none; background:var(--section); border:1px solid var(--divider);
         border-radius:8px; padding:3px 9px; font-size:12px; white-space:nowrap; box-shadow:0 4px 14px rgba(0,0,0,.25); }
  #tip[hidden] { display:none; }
`

// chartTip puts the column's own count next to the cursor. The native title
// tooltip only appears after a delay, and on a phone it never appears at all,
// so the titles move into JS at load — which leaves the page readable with
// scripting off.
// One tooltip element serves every chart on the page; only one of them can be
// under the cursor at a time.
const chartTip = `<div id="tip" hidden></div>
<script>
(function () {
  var charts = document.querySelectorAll('.chart'), tip = document.getElementById('tip')
  if (!charts.length || !tip) return
  function move(e) {
    var col = e.target.closest('.col')
    if (!col) { tip.hidden = true; return }
    tip.textContent = col.dataset.tip
    tip.hidden = false
    var x = e.clientX + 14, y = e.clientY - tip.offsetHeight - 12
    if (x + tip.offsetWidth > innerWidth - 8) x = Math.max(8, e.clientX - tip.offsetWidth - 14)
    if (y < 8) y = e.clientY + 18
    tip.style.transform = 'translate(' + x + 'px,' + y + 'px)'
  }
  charts.forEach(function (chart) {
    chart.querySelectorAll('.col').forEach(function (col) {
      col.dataset.tip = col.title
      col.removeAttribute('title')
    })
    chart.addEventListener('pointermove', move)
    chart.addEventListener('pointerdown', move)
    chart.addEventListener('pointerleave', function () { tip.hidden = true })
  })
})()
</script>`

// sharedCSS is inlined into both pages: the design tokens (TelegramUI-style,
// dark by default, light under <html data-theme="light">) plus the components
// the board and the lead page both use — cards, tags, the score badge, the
// theme switcher.
const sharedCSS = `
  :root { --bg:#020100; --section:#1C1C1D; --input:#2A2A2A; --tertiary:#2A2A2A; --text:#FFFFFF; --hint:#AAAAAA;
          --accent:#2990FF; --divider:rgba(255,255,255,.07); --ok:#32E55E; --bad:#FF5449;
          --card-shadow:none; --wave-off:rgba(255,255,255,.28); }
  :root[data-theme="light"] { --bg:#F3F3F3; --section:#FFFFFF; --input:#EFEFF4; --tertiary:#E9E9EE; --text:#000000; --hint:#707579;
          --accent:#007AFF; --divider:rgba(0,0,0,.08); --ok:#1D9E45; --bad:#E53935;
          --card-shadow:0 1px 2px rgba(0,0,0,.08); --wave-off:rgba(0,0,0,.22); }
  body { margin:0; background:var(--bg); color:var(--text);
         font:15px/1.47 system-ui,-apple-system,BlinkMacSystemFont,"Roboto","Helvetica Neue",sans-serif;
         -webkit-font-smoothing:antialiased; -webkit-tap-highlight-color:transparent; }
  main { max-width:680px; margin:0 auto; padding:16px 16px 56px; }
  a { color:var(--accent); text-decoration:none; }
  a:hover { text-decoration:underline; }
  button { font-family:inherit; }
  .sub { color:var(--hint); }
  .nico { width:14px; height:14px; vertical-align:-2px; }
  .card { background:var(--section); border-radius:12px; box-shadow:var(--card-shadow); min-width:0; }
  .top-bar { display:flex; align-items:flex-start; justify-content:space-between; gap:12px; margin-bottom:12px; }
  .seg { display:flex; background:var(--tertiary); border-radius:9px; padding:2px; gap:2px; flex:0 0 auto; }
  .seg button { border:0; cursor:pointer; font-size:13px; font-weight:600; border-radius:7px; padding:4px 12px; background:none; color:var(--hint); }
  :root:not([data-theme="light"]) .seg .t-dark, :root[data-theme="light"] .seg .t-light
    { background:var(--section); color:var(--text); box-shadow:0 1px 2px rgba(0,0,0,.15); }
  .tag { font-size:12px; font-weight:600; border-radius:999px; padding:2px 10px; margin-left:6px;
         white-space:nowrap; display:inline-block; vertical-align:middle; }
  .tag.new { background:var(--accent); color:#FFFFFF; }
  .tag.applied, .tag.approved { background:color-mix(in srgb, var(--ok) 14%, transparent); color:var(--ok); }
  .tag.rejected { background:color-mix(in srgb, var(--bad) 14%, transparent); color:var(--bad); }
  .tag.prepped, .tag.draft { background:color-mix(in srgb, var(--accent) 15%, transparent); color:var(--accent); }
  .tag.dup { background:none; border:1px solid var(--divider); color:var(--hint); }
  .tag.who-tag { background:var(--tertiary); color:var(--hint); }
  .score { background:color-mix(in srgb, var(--accent) 15%, transparent); color:var(--accent); border-radius:6px;
           padding:1px 7px; font-weight:700; font-size:13px; margin-right:6px; display:inline-block; }
`

// markJS turns every `form.mark` into an optimistic async toggle. The form
// still posts and redirects without JavaScript; with it, the state flips in
// place, the request runs in the background, and a failure flips it back.
// The form's data-state names the class toggled on the enclosing .row (board)
// or <main> (show page); data-mark / data-undo are the two button faces.
// data-carry names an input whose value rides along as `notes` — the approve
// toggle uses it so a caveat typed in the note box just before approving is
// saved with the decision rather than lost.
const markJS = `<script>
document.addEventListener('submit', function (e) {
  var f = e.target
  if (!f.classList || !f.classList.contains('mark') || !f.dataset.state) return
  e.preventDefault()
  var box = f.closest('.row') || f.closest('main')
  var btn = f.querySelector('button')
  var was = box.classList.contains(f.dataset.state)
  var render = function (on) {
    box.classList.toggle(f.dataset.state, on)
    f.classList.toggle('on', on)
    btn.textContent = on ? f.dataset.undo : f.dataset.mark
  }
  render(!was)
  var body
  var carry = f.dataset.carry && document.querySelector(f.dataset.carry)
  if (carry) body = new URLSearchParams({ notes: carry.value })
  fetch(f.action, { method: 'POST', headers: { 'Accept': 'application/json' }, body: body })
    .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json() })
    .then(function (d) { render(d.on) })
    .catch(function () { render(was) })
})
</script>`
