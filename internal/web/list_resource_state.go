package web

import (
	"slices"

	"github.com/kbelokon/readout/internal/web/templates"
)

// resourceStateTableRow removes presentation derived only from the server
// clock while retaining object ResourceVersion. The result is safe to hash for
// both conditional list refreshes and Live projection revisions.
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

func resourceStateListData(data *templates.ListData) templates.ListData {
	semantic := *data
	semantic.Tables = slices.Clone(data.Tables)
	for tableIndex := range semantic.Tables {
		table := &semantic.Tables[tableIndex]
		table.Rows = slices.Clone(table.Rows)
		for rowIndex := range table.Rows {
			table.Rows[rowIndex] = resourceStateTableRow(&table.Rows[rowIndex])
		}
	}
	return semantic
}
