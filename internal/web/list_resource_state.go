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
func resourceStateTableRow(row *templates.TableRow) templates.TableRow {
	semantic := *row
	semantic.Cells = slices.Clone(row.Cells)
	for i := range semantic.Cells {
		cell := semantic.Cells[i]
		if !cell.Volatile {
			continue
		}
		semantic.Cells[i] = templates.TableCell{
			Kind:     cell.Kind,
			ColClass: cell.ColClass,
			Volatile: true,
		}
	}
	return semantic
}
