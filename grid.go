package main

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"

	svg "github.com/ajstarks/svgo"
	"github.com/lsnnt/spotify-banner-for-github/pathsconst"
)

// Cover-wall grid geometry. The canvas holds as many fixed-size cells per row
// as the requested width allows; the height grows with the number of rows.
const (
	gridCell       = 128                     // cover edge length, px
	gridGap        = 16                      // horizontal gap between cells, px
	gridMargin     = 24                      // left/right canvas margin, px
	gridTop        = 110                     // space reserved for the logo + header
	gridLabelH     = 36                      // track name + artist text lines under a cover
	gridBottom     = 24                      // bottom margin, px
	maxGridTracks  = 12                      // cap on rendered covers
	minGridWidth   = 2*gridMargin + gridCell // 176px: below this even one cell does not fit
	maxGridWidth   = 2000
	defaultWidth   = 700
	maxNameRunes   = 18
	maxArtistRunes = 20
)

// parseViewParams reads the optional ?grid= and ?width= query parameters.
// A grid canvas narrower than one cell is rejected with a clear error.
func parseViewParams(r *http.Request) (grid bool, width int, err error) {
	q := r.URL.Query()
	switch q.Get("grid") {
	case "1", "true", "yes":
		grid = true
	}
	width = defaultWidth
	if ws := q.Get("width"); ws != "" {
		width, err = strconv.Atoi(ws)
		if err != nil {
			return false, 0, fmt.Errorf("invalid width %q: must be an integer number of pixels", ws)
		}
	}
	if grid {
		if width < minGridWidth {
			return false, 0, fmt.Errorf("canvas too narrow for cover grid: width %dpx, minimum is %dpx", width, minGridWidth)
		}
		if width > maxGridWidth {
			width = maxGridWidth
		}
	}
	return grid, width, nil
}

// gridLayout is the computed geometry of a cover wall.
type gridLayout struct {
	cols   int
	rows   int
	width  int
	height int
}

// computeGridLayout derives columns/rows/canvas height for n cells.
func computeGridLayout(width, n int) gridLayout {
	cols := (width - 2*gridMargin + gridGap) / (gridCell + gridGap)
	if cols < 1 {
		cols = 1
	}
	rows := (n + cols - 1) / cols
	height := gridTop + rows*(gridCell+gridLabelH) + (rows-1)*gridGap + gridBottom
	return gridLayout{cols: cols, rows: rows, width: width, height: height}
}

// cellOrigin returns the top-left corner of cell i.
func (l gridLayout) cellOrigin(i int) (x, y int) {
	col, row := i%l.cols, i/l.cols
	x = gridMargin + col*(gridCell+gridGap)
	y = gridTop + row*(gridCell+gridLabelH+gridGap)
	return x, y
}

// truncateRunes shortens s to at most max runes, adding an ellipsis.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// renderGrid draws the cover wall: deduplicated recent tracks in a grid of
// embedded covers with the track name and artist under each cell. Covers are
// fetched concurrently; any that fail validation get a placeholder tile.
func renderGrid(w http.ResponseWriter, tracks []TrackInfo, width int) {
	tracks = dedupeTracks(tracks)
	if len(tracks) > maxGridTracks {
		tracks = tracks[:maxGridTracks]
	}

	s := svg.New(w)
	if len(tracks) == 0 {
		s.Start(width, 200)
		s.Rect(0, 0, width, 200, "fill:#79A7F2")
		s.Text(width/2, 100, "No recent tracks found", "text-anchor:middle;font-family:Arial, sans-serif;font-size:20px;")
		s.End()
		return
	}

	// Fetch all covers concurrently so one slow host can't serialize the render.
	uris := make([]string, len(tracks))
	ok := make([]bool, len(tracks))
	var wg sync.WaitGroup
	for i, t := range tracks {
		wg.Add(1)
		go func(i int, coverURL string) {
			defer wg.Done()
			uris[i], ok[i] = fetchCoverDataURI(coverURL)
		}(i, t.CoverURL)
	}
	wg.Wait()

	lay := computeGridLayout(width, len(tracks))
	s.Start(lay.width, lay.height)
	s.Rect(0, 0, lay.width, lay.height, "fill:#79A7F2")
	s.Image(17, 2, 100, 100, pathsconst.Imagelogo())
	s.Path(pathsconst.Topheader())

	for i, t := range tracks {
		x, y := lay.cellOrigin(i)
		if ok[i] {
			s.Image(x, y, gridCell, gridCell, uris[i])
		} else {
			// Placeholder tile for missing/invalid covers.
			s.Rect(x, y, gridCell, gridCell, "fill:#E0E0E0;stroke:#888888;stroke-width:1")
			s.Text(x+gridCell/2, y+gridCell/2+12, "♪", "text-anchor:middle;font-family:Arial, sans-serif;font-size:36px;fill:#888888")
		}
		s.Text(x, y+gridCell+16, truncateRunes(t.Name, maxNameRunes), "font-family:Arial, sans-serif;font-size:13px;fill:#000000")
		artist := t.Artist
		if artist == "" {
			artist = "Unknown artist"
		}
		s.Text(x, y+gridCell+32, truncateRunes(artist, maxArtistRunes), "font-family:Arial, sans-serif;font-size:11px;fill:#222222")
	}
	s.End()
}
