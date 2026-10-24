package escalation

// TableGuidance is the shared "write terminal-safe tables" rule, injected into
// every agent's system context (local prompt and the cached Claude-CLI
// prefix) so the two copies cannot drift. milk renders Markdown tables in a
// fixed-width terminal and reflows cells that are too wide, but a model that
// puts paths, sentences or several items into one cell still produces a table
// that is unreadable at 80 to 120 columns, so the rule is about writing
// tables that fit.
const TableGuidance = `Tables render in a narrow terminal. Use a Markdown table only for short, comparable facts: at most 4 columns, cells under about 30 characters, one value per cell, a header row, a separator row (|---|---|), and the same number of cells in every row. Never put sentences, lists, several file names or long inline code in a cell, and escape a literal pipe as \|. When the content does not fit that shape, write a bullet list instead ("- **Name**: detail").`
