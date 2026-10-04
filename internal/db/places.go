package db

import (
	"context"
	"database/sql"
)

// Regione and Provincia: the Italian regions and provinces, the dictionaries
// for the customers' "regione" and "provincia" (which store the name).
type Regione struct {
	ID   int // ISTAT code
	Nome string
}

type Provincia struct {
	Sigla     string // "VI"
	Nome      string // "Vicenza"
	RegioneID int
}

var Regioni = []Regione{
	{1, "Piemonte"}, {2, "Valle d'Aosta"}, {3, "Lombardia"}, {4, "Trentino-Alto Adige"},
	{5, "Veneto"}, {6, "Friuli-Venezia Giulia"}, {7, "Liguria"}, {8, "Emilia-Romagna"},
	{9, "Toscana"}, {10, "Umbria"}, {11, "Marche"}, {12, "Lazio"}, {13, "Abruzzo"},
	{14, "Molise"}, {15, "Campania"}, {16, "Puglia"}, {17, "Basilicata"}, {18, "Calabria"},
	{19, "Sicilia"}, {20, "Sardegna"},
}

var Province = []Provincia{
	{"TO", "Torino", 1}, {"VC", "Vercelli", 1}, {"NO", "Novara", 1}, {"CN", "Cuneo", 1},
	{"AT", "Asti", 1}, {"AL", "Alessandria", 1}, {"BI", "Biella", 1}, {"VB", "Verbano-Cusio-Ossola", 1},
	{"AO", "Aosta", 2},
	{"VA", "Varese", 3}, {"CO", "Como", 3}, {"SO", "Sondrio", 3}, {"MI", "Milano", 3},
	{"BG", "Bergamo", 3}, {"BS", "Brescia", 3}, {"PV", "Pavia", 3}, {"CR", "Cremona", 3},
	{"MN", "Mantova", 3}, {"LC", "Lecco", 3}, {"LO", "Lodi", 3}, {"MB", "Monza e della Brianza", 3},
	{"BZ", "Bolzano", 4}, {"TN", "Trento", 4},
	{"VR", "Verona", 5}, {"VI", "Vicenza", 5}, {"BL", "Belluno", 5}, {"TV", "Treviso", 5},
	{"VE", "Venezia", 5}, {"PD", "Padova", 5}, {"RO", "Rovigo", 5},
	{"UD", "Udine", 6}, {"GO", "Gorizia", 6}, {"TS", "Trieste", 6}, {"PN", "Pordenone", 6},
	{"IM", "Imperia", 7}, {"SV", "Savona", 7}, {"GE", "Genova", 7}, {"SP", "La Spezia", 7},
	{"PC", "Piacenza", 8}, {"PR", "Parma", 8}, {"RE", "Reggio Emilia", 8}, {"MO", "Modena", 8},
	{"BO", "Bologna", 8}, {"FE", "Ferrara", 8}, {"RA", "Ravenna", 8}, {"FC", "Forlì-Cesena", 8},
	{"RN", "Rimini", 8},
	{"MS", "Massa-Carrara", 9}, {"LU", "Lucca", 9}, {"PT", "Pistoia", 9}, {"FI", "Firenze", 9},
	{"LI", "Livorno", 9}, {"PI", "Pisa", 9}, {"AR", "Arezzo", 9}, {"SI", "Siena", 9},
	{"GR", "Grosseto", 9}, {"PO", "Prato", 9},
	{"PG", "Perugia", 10}, {"TR", "Terni", 10},
	{"PU", "Pesaro e Urbino", 11}, {"AN", "Ancona", 11}, {"MC", "Macerata", 11},
	{"AP", "Ascoli Piceno", 11}, {"FM", "Fermo", 11},
	{"VT", "Viterbo", 12}, {"RI", "Rieti", 12}, {"RM", "Roma", 12}, {"LT", "Latina", 12}, {"FR", "Frosinone", 12},
	{"AQ", "L'Aquila", 13}, {"TE", "Teramo", 13}, {"PE", "Pescara", 13}, {"CH", "Chieti", 13},
	{"CB", "Campobasso", 14}, {"IS", "Isernia", 14},
	{"CE", "Caserta", 15}, {"BN", "Benevento", 15}, {"NA", "Napoli", 15}, {"AV", "Avellino", 15}, {"SA", "Salerno", 15},
	{"FG", "Foggia", 16}, {"BA", "Bari", 16}, {"TA", "Taranto", 16}, {"BR", "Brindisi", 16},
	{"LE", "Lecce", 16}, {"BT", "Barletta-Andria-Trani", 16},
	{"PZ", "Potenza", 17}, {"MT", "Matera", 17},
	{"CS", "Cosenza", 18}, {"CZ", "Catanzaro", 18}, {"RC", "Reggio Calabria", 18},
	{"KR", "Crotone", 18}, {"VV", "Vibo Valentia", 18},
	{"TP", "Trapani", 19}, {"PA", "Palermo", 19}, {"ME", "Messina", 19}, {"AG", "Agrigento", 19},
	{"CL", "Caltanissetta", 19}, {"EN", "Enna", 19}, {"CT", "Catania", 19}, {"RG", "Ragusa", 19},
	{"SR", "Siracusa", 19},
	{"SS", "Sassari", 20}, {"NU", "Nuoro", 20}, {"CA", "Cagliari", 20}, {"OR", "Oristano", 20},
	{"SU", "Sud Sardegna", 20},
}

const placesSchema = `
CREATE TABLE IF NOT EXISTS regioni (
    id   INTEGER PRIMARY KEY, -- ISTAT code
    nome TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS province (
    sigla      TEXT PRIMARY KEY, -- "VI"
    nome       TEXT NOT NULL UNIQUE,
    regione_id INTEGER NOT NULL REFERENCES regioni(id)
);`

// seedPlaces creates the dictionaries and adds the entries they miss
// (existing rows are left as they are).
func (s *Store) seedPlaces(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, placesSchema); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertAll(ctx, tx, "INSERT OR IGNORE INTO regioni (id, nome) VALUES (?, ?)", len(Regioni), func(i int) []any {
		return []any{Regioni[i].ID, Regioni[i].Nome}
	}); err != nil {
		return err
	}
	if err := insertAll(ctx, tx, "INSERT OR IGNORE INTO province (sigla, nome, regione_id) VALUES (?, ?, ?)", len(Province), func(i int) []any {
		return []any{Province[i].Sigla, Province[i].Nome, Province[i].RegioneID}
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func insertAll(ctx context.Context, tx *sql.Tx, stmt string, n int, args func(int) []any) error {
	st, err := tx.PrepareContext(ctx, stmt)
	if err != nil {
		return err
	}
	defer st.Close()
	for i := 0; i < n; i++ {
		if _, err := st.ExecContext(ctx, args(i)...); err != nil {
			return err
		}
	}
	return nil
}
