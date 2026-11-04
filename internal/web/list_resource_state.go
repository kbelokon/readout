package web

import (
	"slices"

	"github.com/kbelokon/readout/internal/web/templates"
)

// resourceStateTableRow removes presentation derived only from the server clock
// while retaining object ResourceVersion. It backs the Live projection's row
// digest: a cell whose text is a function of "now" would otherwise push a row
// upsert every poll for a row nothing happened to. The conditional-refresh
// validator deliberately does NOT use it -- see resourceListETag.
//
// A clock cell (see clock_cells.go) keeps its object timestamps, its kind and
// every part that is not generated from the clock -- a restart count stays --
// and drops the generated age text and the .age-* colour; the browser advances
// those itself. The row's Created colour is clock-derived the same way, while
// its absolute text is not. A legacy Volatile cell (an Event age without a
// decodable timestamp) is dropped whole.
func resourceStateTableRow(row *templates.TableRow) templates.TableRow {
	semantic := *row
	semantic.CreatedClass = ""
	semantic.Cells = slices.Clone(row.Cells)
	for i := range semantic.Cells {
		cell := semantic.Cells[i]
		switch {
		case cell.Volatile:
			semantic.Cells[i] = templates.TableCell{
				Kind:     cell.Kind,
				ColClass: cell.ColClass,
				Volatile: true,
			}
		case cell.Clock.Format != "":
			if cell.Kind != templates.CellRestarts {
				cell.Value = ""
			}
			cell.Class = ""
			cell.Ago = ""
			cell.EvAgeRest = ""
			semantic.Cells[i] = cell
		}
	}
	return semantic
}
