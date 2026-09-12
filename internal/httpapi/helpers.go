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
const pageHead = `<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<link rel="icon" type="image/svg+xml" href="{{.Base}}/favicon.svg">
<link rel="icon" type="image/png" sizes="32x32" href="{{.Base}}/favicon.png">
<link rel="apple-touch-icon" href="{{.Base}}/apple-touch-icon.png">`

// chartCSS styles the day/week column chart and the tooltip that follows the
// cursor across it.
const chartCSS = `
  .chart { display:flex; align-items:flex-end; gap:2px; height:88px; border-bottom:1px solid #2a2a33; }
  .chart .col { flex:1 1 0; min-width:2px; height:100%; display:flex; align-items:flex-end; }
  .chart .bar { width:100%; background:#3b6ea5; border-radius:2px 2px 0 0; }
  .chart .col:hover .bar { background:#7db5ff; }
  .axis { display:flex; justify-content:space-between; color:#8b8b96; font-size:11px; margin:4px 0 22px; }
  #tip { position:fixed; top:0; left:0; z-index:9; pointer-events:none; background:#1b1b22; border:1px solid #3a3a46;
         border-radius:6px; padding:3px 8px; font-size:12px; white-space:nowrap; box-shadow:0 2px 10px rgba(0,0,0,.45); }
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

// sharedCSS is inlined into both pages. The live dot pulses so an open tab
// tells you at a glance whether the visitor is still there.
const sharedCSS = `
  body { background:#101014; color:#e8e8ec; font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace; margin:0; padding:24px; }
  .nico { width:15px; height:15px; vertical-align:-3px; }
  main { max-width:720px; margin:0 auto; }
  h1 { font-size:18px; margin:0 0 2px; word-break:break-all; }
  a { color:#7db5ff; text-decoration:none; }
  a:hover { text-decoration:underline; }
  .tag { font-size:12px; font-weight:600; border-radius:4px; padding:1px 7px; vertical-align:middle;
         white-space:nowrap; display:inline-block; margin-left:6px; }
  .tag.first { color:#101014; background:#8b8b96; }
  .tag.return { color:#101014; background:#5cd58c; }
  .sub { color:#8b8b96; }
  .dot { display:inline-block; width:8px; height:8px; border-radius:50%; background:#5cd58c; margin-right:6px; vertical-align:middle; animation:pulse 1.6s ease-in-out infinite; }
  @keyframes pulse { 0%,100% { opacity:1 } 50% { opacity:.25 } }
  .now { color:#5cd58c; }
  @media (prefers-reduced-motion: reduce) { .dot { animation:none } }
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
