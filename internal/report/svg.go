// Package report renders results as a terminal summary and a
// self-contained HTML report with inline SVG charts.
//
// Charts follow the dataviz rules adopted for PathGauge: one y-axis per
// chart (two measures become two charts), categorical colours in a fixed
// validated order, 2px lines, a legend whenever there are two or more
// series, recessive grid, hover tooltips, and a separately chosen dark
// palette.
package report

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"math"
	"strings"
)

// Series is one line on a chart. Slot picks the categorical colour (1..8).
type Series struct {
	Name   string
	X, Y   []float64
	Slot   int
	Points bool // draw markers only (no connecting line)
}

// Band shades an x-range (e.g. a test phase).
type Band struct {
	From, To float64
	Label    string
}

// LineChart is an x/y chart with one y-axis.
type LineChart struct {
	ID            string
	Title         string
	XLabel        string
	YLabel        string
	Series        []Series
	Bands         []Band
	YZero         bool // start the y-axis at zero
	Width, Height int
	XLog          bool
}

const (
	padL, padR, padT, padB = 56, 16, 12, 40
)

func niceTicks(lo, hi float64, n int) []float64 {
	if hi <= lo {
		hi = lo + 1
	}
	span := hi - lo
	step := math.Pow(10, math.Floor(math.Log10(span/float64(n))))
	for _, m := range []float64{1, 2, 5, 10} {
		if span/(step*m) <= float64(n) {
			step *= m
			break
		}
	}
	start := math.Floor(lo/step) * step
	var out []float64
	for v := start; v <= hi+step*1e-9; v += step {
		if v >= lo-step*1e-9 {
			out = append(out, math.Round(v/step)*step)
		}
	}
	return out
}

func fmtTick(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e6:
		return trim(fmt.Sprintf("%.1fM", v/1e6))
	case a >= 1e4:
		return trim(fmt.Sprintf("%.0fk", v/1e3))
	case a >= 100 || v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	case a >= 10:
		return trim(fmt.Sprintf("%.1f", v))
	}
	return trim(fmt.Sprintf("%.2f", v))
}

func trim(s string) string {
	if strings.Contains(s, ".") && !strings.ContainsAny(s, "eE") {
		suffix := ""
		if n := len(s); n > 0 && (s[n-1] == 'M' || s[n-1] == 'k') {
			suffix, s = s[n-1:], s[:n-1]
		}
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		return s + suffix
	}
	return s
}

// SVG renders the chart.
func (c LineChart) SVG() template.HTML {
	w, h := c.Width, c.Height
	if w == 0 {
		w = 720
	}
	if h == 0 {
		h = 240
	}
	xmin, xmax := math.Inf(1), math.Inf(-1)
	ymin, ymax := math.Inf(1), math.Inf(-1)
	for _, s := range c.Series {
		for i := range s.X {
			if i >= len(s.Y) || math.IsNaN(s.Y[i]) || s.Y[i] < 0 && c.YZero {
				continue
			}
			x := s.X[i]
			if c.XLog && x <= 0 {
				continue
			}
			xmin, xmax = math.Min(xmin, x), math.Max(xmax, x)
			ymin, ymax = math.Min(ymin, s.Y[i]), math.Max(ymax, s.Y[i])
		}
	}
	if math.IsInf(xmin, 0) {
		return template.HTML(`<p class="muted">No data to chart.</p>`)
	}
	if c.YZero || ymin > 0 && ymin < (ymax-ymin)*0.5 {
		ymin = math.Min(0, ymin)
	}
	if ymax == ymin {
		ymax = ymin + 1
	}
	ymax += (ymax - ymin) * 0.06
	tx := func(x float64) float64 {
		if c.XLog {
			return padL + (math.Log10(x)-math.Log10(xmin))/math.Max(math.Log10(xmax)-math.Log10(xmin), 1e-9)*float64(w-padL-padR)
		}
		return padL + (x-xmin)/math.Max(xmax-xmin, 1e-9)*float64(w-padL-padR)
	}
	ty := func(y float64) float64 { return float64(h-padB) - (y-ymin)/(ymax-ymin)*float64(h-padT-padB) }

	var b strings.Builder
	fmt.Fprintf(&b, `<figure class="chart" data-chart="line"><figcaption>%s</figcaption>`, html.EscapeString(c.Title))
	if len(c.Series) > 1 {
		b.WriteString(`<div class="legend">`)
		for _, s := range c.Series {
			fmt.Fprintf(&b, `<span><i class="sw s%d%s"></i>%s</span>`, slot(s.Slot), map[bool]string{true: " dot", false: ""}[s.Points], html.EscapeString(s.Name))
		}
		b.WriteString(`</div>`)
	}
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" role="img" aria-label="%s" preserveAspectRatio="xMidYMid meet">`, w, h, html.EscapeString(c.Title))
	for _, bd := range c.Bands {
		x1, x2 := tx(math.Max(bd.From, xmin)), tx(math.Min(bd.To, xmax))
		if x2 <= x1 {
			continue
		}
		fmt.Fprintf(&b, `<rect class="band" x="%.1f" y="%d" width="%.1f" height="%d"/><text class="bandlabel" x="%.1f" y="%d">%s</text>`,
			x1, padT, x2-x1, h-padT-padB, x1+4, padT+12, html.EscapeString(bd.Label))
	}
	for _, t := range niceTicks(ymin, ymax, 5) {
		y := ty(t)
		fmt.Fprintf(&b, `<line class="grid" x1="%d" x2="%d" y1="%.1f" y2="%.1f"/><text class="tick" x="%d" y="%.1f" text-anchor="end">%s</text>`,
			padL, w-padR, y, y, padL-6, y+4, fmtTick(t))
	}
	var xt []float64
	if c.XLog {
		for p := math.Floor(math.Log10(xmin)); p <= math.Ceil(math.Log10(xmax)); p++ {
			for _, m := range []float64{1, 2, 5} {
				v := m * math.Pow(10, p)
				if v >= xmin && v <= xmax {
					xt = append(xt, v)
				}
			}
		}
	} else {
		xt = niceTicks(xmin, xmax, 8)
	}
	for _, t := range xt {
		x := tx(t)
		fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%d" text-anchor="middle">%s</text>`, x, h-padB+16, fmtTick(t))
	}
	fmt.Fprintf(&b, `<line class="axis" x1="%d" x2="%d" y1="%d" y2="%d"/>`, padL, w-padR, h-padB, h-padB)
	fmt.Fprintf(&b, `<text class="axislabel" x="%d" y="%d" text-anchor="middle">%s</text>`, (w+padL)/2, h-6, html.EscapeString(c.XLabel))
	fmt.Fprintf(&b, `<text class="axislabel" transform="translate(14 %d) rotate(-90)" text-anchor="middle">%s</text>`, (h-padB+padT)/2, html.EscapeString(c.YLabel))

	type hov struct {
		Name string       `json:"n"`
		Pts  [][2]float64 `json:"p"`
	}
	var hdata []hov
	for _, s := range c.Series {
		var pts []string
		var hp [][2]float64
		for i := range s.X {
			if i >= len(s.Y) || math.IsNaN(s.Y[i]) || (c.XLog && s.X[i] <= 0) {
				continue
			}
			if s.Y[i] < 0 && c.YZero { // gaps (e.g. lost probes)
				if !s.Points && len(pts) > 1 {
					fmt.Fprintf(&b, `<polyline class="line s%d" points="%s"/>`, slot(s.Slot), strings.Join(pts, " "))
				}
				pts = nil
				continue
			}
			x, y := tx(s.X[i]), ty(s.Y[i])
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
			hp = append(hp, [2]float64{s.X[i], s.Y[i]})
			if s.Points {
				fmt.Fprintf(&b, `<circle class="pt s%d" cx="%.1f" cy="%.1f" r="2.2"/>`, slot(s.Slot), x, y)
			}
		}
		if !s.Points && len(pts) > 1 {
			fmt.Fprintf(&b, `<polyline class="line s%d" points="%s"/>`, slot(s.Slot), strings.Join(pts, " "))
		} else if !s.Points && len(pts) == 1 {
			fmt.Fprintf(&b, `<circle class="pt s%d" cx="%s" r="4"/>`, slot(s.Slot), strings.Replace(pts[0], ",", `" cy="`, 1))
		}
		hdata = append(hdata, hov{Name: s.Name, Pts: thin(hp, 600)})
	}
	js, _ := json.Marshal(map[string]any{"s": hdata, "x": []float64{xmin, xmax}, "y": []float64{ymin, ymax}, "w": w, "h": h, "log": c.XLog,
		"pad": []int{padL, padR, padT, padB}, "xl": c.XLabel, "yl": c.YLabel})
	fmt.Fprintf(&b, `<line class="xhair" x1="0" x2="0" y1="%d" y2="%d" visibility="hidden"/>`, padT, h-padB)
	b.WriteString(`</svg>`)
	fmt.Fprintf(&b, `<div class="tip" hidden></div><script type="application/json" class="hoverdata">%s</script></figure>`, js) // json.Marshal escapes <, > and &
	return template.HTML(b.String())
}

// thin keeps at most n evenly spaced points for hover data.
func thin(p [][2]float64, n int) [][2]float64 {
	if len(p) <= n {
		return p
	}
	out := make([][2]float64, 0, n)
	step := float64(len(p)) / float64(n)
	for i := 0; i < n; i++ {
		out = append(out, p[int(float64(i)*step)])
	}
	return out
}

func slot(s int) int {
	if s < 1 || s > 8 {
		return 1
	}
	return s
}

// BarSeries is one group member in a bar chart.
type BarSeries struct {
	Name   string
	Values []float64
	Slot   int
}

// BarChart is a vertical (grouped) bar chart.
type BarChart struct {
	Title      string
	YLabel     string
	XLabel     string
	Categories []string
	Series     []BarSeries
	Width      int
	Height     int
	Unit       string
}

// SVG renders the bar chart.
func (c BarChart) SVG() template.HTML {
	w, h := c.Width, c.Height
	if w == 0 {
		w = 720
	}
	if h == 0 {
		h = 220
	}
	ymax := 0.0
	for _, s := range c.Series {
		for _, v := range s.Values {
			ymax = math.Max(ymax, v)
		}
	}
	if ymax == 0 {
		ymax = 1
	}
	ymax *= 1.12
	ty := func(y float64) float64 { return float64(h-padB) - y/ymax*float64(h-padT-padB) }
	var b strings.Builder
	fmt.Fprintf(&b, `<figure class="chart" data-chart="bar"><figcaption>%s</figcaption>`, html.EscapeString(c.Title))
	if len(c.Series) > 1 {
		b.WriteString(`<div class="legend">`)
		for _, s := range c.Series {
			fmt.Fprintf(&b, `<span><i class="sw s%d"></i>%s</span>`, slot(s.Slot), html.EscapeString(s.Name))
		}
		b.WriteString(`</div>`)
	}
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" role="img" aria-label="%s">`, w, h, html.EscapeString(c.Title))
	for _, t := range niceTicks(0, ymax, 4) {
		y := ty(t)
		fmt.Fprintf(&b, `<line class="grid" x1="%d" x2="%d" y1="%.1f" y2="%.1f"/><text class="tick" x="%d" y="%.1f" text-anchor="end">%s</text>`, padL, w-padR, y, y, padL-6, y+4, fmtTick(t))
	}
	n := len(c.Categories)
	groupW := float64(w-padL-padR) / float64(max(n, 1))
	ns := max(len(c.Series), 1)
	barW := math.Min(48, (groupW-12)/float64(ns)-2)
	for i, cat := range c.Categories {
		gx := float64(padL) + groupW*float64(i) + (groupW-(barW+2)*float64(ns))/2
		for j, s := range c.Series {
			if i >= len(s.Values) {
				continue
			}
			v := s.Values[i]
			x := gx + float64(j)*(barW+2)
			y := ty(v)
			hgt := float64(h-padB) - y
			r := math.Min(4, math.Min(barW/2, hgt))
			// Rounded data end, square at the baseline.
			fmt.Fprintf(&b, `<path class="bar s%d" d="M%.1f,%d V%.1f Q%.1f,%.1f %.1f,%.1f H%.1f Q%.1f,%.1f %.1f,%.1f V%d Z"><title>%s %s: %s %s</title></path>`,
				slot(s.Slot), x, h-padB, y+r, x, y, x+r, y, x+barW-r, x+barW, y, x+barW, y+r, h-padB,
				html.EscapeString(s.Name), html.EscapeString(cat), fmtTick(v), html.EscapeString(c.Unit))
			if ns <= 2 && n <= 12 {
				fmt.Fprintf(&b, `<text class="vlabel" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, x+barW/2, y-4, fmtTick(v))
			}
		}
		fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%d" text-anchor="middle">%s</text>`, float64(padL)+groupW*(float64(i)+0.5), h-padB+16, html.EscapeString(cat))
	}
	fmt.Fprintf(&b, `<line class="axis" x1="%d" x2="%d" y1="%d" y2="%d"/>`, padL, w-padR, h-padB, h-padB)
	fmt.Fprintf(&b, `<text class="axislabel" x="%d" y="%d" text-anchor="middle">%s</text>`, (w+padL)/2, h-6, html.EscapeString(c.XLabel))
	fmt.Fprintf(&b, `<text class="axislabel" transform="translate(14 %d) rotate(-90)" text-anchor="middle">%s</text>`, (h-padB+padT)/2, html.EscapeString(c.YLabel))
	b.WriteString(`</svg></figure>`)
	return template.HTML(b.String())
}
