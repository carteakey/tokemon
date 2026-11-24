package api

import (
	"fmt"
	"html"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/database"
)

func (s *Server) shareCardSVG(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	overview, _, err := s.cachedOverview(r.Context())
	s.metrics.observeDB(started)
	if err != nil {
		if clientGone(err) {
			return
		}
		http.Error(w, "card unavailable", http.StatusInternalServerError)
		return
	}
	svg := renderShareCardSVG(overview)
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(svg))
}

func renderShareCardSVG(overview database.Overview) string {
	var recentDays []database.ActivityDay
	for _, week := range overview.Activity.Weeks {
		for _, day := range week.Days {
			if !day.Future {
				recentDays = append(recentDays, day)
			}
		}
	}
	if len(recentDays) > 30 {
		recentDays = recentDays[len(recentDays)-30:]
	}
	var maxDaily int64
	for _, day := range recentDays {
		if day.Tokens > maxDaily {
			maxDaily = day.Tokens
		}
	}

	progress := overview.Evolution.Progress
	if math.IsNaN(progress) || progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}
	barWidth := int(progress * 260)
	if progress > 0 && barWidth < 4 {
		barWidth = 4
	}

	remainingLabel := "Final form"
	if overview.Evolution.TokensRemaining != nil {
		remainingLabel = compact(*overview.Evolution.TokensRemaining) + " to next"
	}

	formName := strings.TrimSpace(overview.Evolution.FormName)
	if formName == "" {
		formName = "Token Egg"
	}

	lifetimeLabel := commas(overview.LifetimeTokens)
	stageLabel := fmt.Sprintf("FORM / %02d", overview.Evolution.Stage)
	activeLabel := fmt.Sprintf("%s active d", commas(overview.Activity.ActiveDays))
	streakLabel := fmt.Sprintf("%dd streak · %dd best", overview.Activity.CurrentStreak, overview.Activity.LongestStreak)
	progressPct := fmt.Sprintf("%.0f%%", progress*100)

	title := fmt.Sprintf("Tokemon share card: %s, %s lifetime tokens", formName, lifetimeLabel)
	desc := fmt.Sprintf("Stage %d %s with %s lifetime tokens, %s, and %s.",
		overview.Evolution.Stage, formName, lifetimeLabel, activeLabel, streakLabel)

	var spark strings.Builder
	const (
		sparkX      = 338
		sparkY      = 128
		sparkWidth  = 274
		sparkHeight = 52
		barStep     = 9
		barW        = 7
	)
	offset := 30 - len(recentDays)
	for i := 0; i < 30; i++ {
		x := sparkX + i*barStep
		h := 3
		fill := "#262b23"
		stroke := "#30352d"
		if i >= offset {
			day := recentDays[i-offset]
			if maxDaily > 0 && day.Tokens > 0 {
				h = int(float64(day.Tokens) / float64(maxDaily) * float64(sparkHeight-4))
				if h < 4 {
					h = 4
				}
				switch {
				case day.Level >= 4:
					fill, stroke = "#9bb76d", "#aecb79"
				case day.Level == 3:
					fill, stroke = "#6b8d56", "#779b5f"
				case day.Level == 2:
					fill, stroke = "#416b46", "#4f724c"
				default:
					fill, stroke = "#2a4430", "#36533b"
				}
			} else if day.UnknownTokens > 0 {
				h = 4
				fill, stroke = "#1d201b", "#d2a477"
			}
		}
		y := sparkY + sparkHeight - h
		fmt.Fprintf(&spark, `<rect x="%d" y="%d" width="%d" height="%d" rx="1" fill="%s" stroke="%s" stroke-width="1"/>`,
			x, y, barW, h, fill, stroke)
	}

	crest := stageCrestSVG(overview.Evolution.Stage)

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="640" height="220" viewBox="0 0 640 220" role="img" aria-label="%s">
  <title>%s</title>
  <desc>%s</desc>
  <rect x="0.5" y="0.5" width="639" height="219" rx="12" fill="#10110f" stroke="#30352d"/>
  <rect x="10.5" y="10.5" width="619" height="199" rx="8" fill="#171916" stroke="#485044"/>
  <rect x="22.5" y="22.5" width="76" height="76" rx="6" fill="#1d201b" stroke="#485044"/>
  %s
  <text x="114" y="37" fill="#9bbba0" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="10" font-weight="700" letter-spacing="1.4">TOKEMON FIELD CARD</text>
  <text x="114" y="64" fill="#f0ede5" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="22" font-weight="700">%s</text>
  <rect x="114" y="74" width="84" height="20" rx="4" fill="#1d201b" stroke="#d2a477"/>
  <text x="156" y="88" text-anchor="middle" fill="#d2a477" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="10" font-weight="700" letter-spacing="0.8">%s</text>
  <text x="612" y="37" text-anchor="end" fill="#a2a69b" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="10">%s</text>
  <text x="612" y="66" text-anchor="end" fill="#f0ede5" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="28" font-weight="700">%s</text>
  <text x="612" y="84" text-anchor="end" fill="#9bbba0" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="10" letter-spacing="1.0">LIFETIME TOKENS</text>
  <line x1="22" y1="110" x2="618" y2="110" stroke="#30352d"/>
  <text x="22" y="130" fill="#9bbba0" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="10" font-weight="700" letter-spacing="1.0">EVOLUTION</text>
  <text x="282" y="130" text-anchor="end" fill="#d2a477" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="10" font-weight="700">%s · %s</text>
  <rect x="22.5" y="139.5" width="260" height="14" rx="4" fill="#10110f" stroke="#485044"/>
  <rect x="23" y="140" width="%d" height="13" rx="3" fill="#9bbba0"/>
  <text x="22" y="174" fill="#a2a69b" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="11">%s</text>
  <text x="22" y="192" fill="#f0ede5" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="11" font-weight="700">%s</text>
  <text x="338" y="124" fill="#9bbba0" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="10" font-weight="700" letter-spacing="1.0">LAST 30 DAYS</text>
  <text x="612" y="124" text-anchor="end" fill="#a2a69b" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="10">AGGREGATE ONLY</text>
  %s
  <text x="338" y="196" fill="#a2a69b" font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="9">Local-first · no projects, machines, or prompts</text>
</svg>
`,
		html.EscapeString(title),
		html.EscapeString(title),
		html.EscapeString(desc),
		crest,
		html.EscapeString(formName),
		html.EscapeString(stageLabel),
		html.EscapeString(activeLabel),
		html.EscapeString(lifetimeLabel),
		html.EscapeString(progressPct),
		html.EscapeString(remainingLabel),
		barWidth,
		html.EscapeString(activeLabel),
		html.EscapeString(streakLabel),
		spark.String(),
	)
}

func stageCrestSVG(stage int) string {
	if stage < 1 {
		stage = 1
	}
	if stage > 10 {
		stage = 10
	}
	// Deterministic 5x5 symmetric pixel crest inside the 76x76 badge (x=22.5, y=22.5).
	const (
		originX = 35
		originY = 35
		cell    = 10
	)
	var b strings.Builder
	for row := 0; row < 5; row++ {
		for col := 0; col < 5; col++ {
			mirrorCol := col
			if mirrorCol > 2 {
				mirrorCol = 4 - col
			}
			bit := ((stage*7 + row*5 + mirrorCol*3) % 5)
			on := (row == 2 && mirrorCol == 2) || (bit < 2+(stage%2))
			if !on {
				continue
			}
			fill := "#607864"
			if row == 1 || (row == 2 && mirrorCol == 2) {
				fill = "#9bbba0"
			}
			if row == 0 && mirrorCol == 2 && stage >= 4 {
				fill = "#d2a477"
			}
			fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" fill="%s"/>`,
				originX+col*cell, originY+row*cell, cell-1, cell-1, fill)
		}
	}
	return b.String()
}
