package service

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"adesgo/internal/db"
)

// Provincia: an entry of the provinces dictionary, with its region's name.
type Provincia struct {
	Sigla, Nome, Regione string
}

// Province lists the provinces by name; Regioni the regions by name (the
// dictionaries seeded in the regioni / province tables).
func Province() []Provincia { return province }
func Regioni() []string     { return regioni }

var (
	province  []Provincia
	regioni   []string
	provByKey = map[string]Provincia{} // by name and by sigla
	regByKey  = map[string]string{}
)

func init() {
	byID := map[int]string{}
	for _, r := range db.Regioni {
		byID[r.ID] = r.Nome
		regioni = append(regioni, r.Nome)
		regByKey[dictKey(r.Nome)] = r.Nome
	}
	for _, p := range db.Province {
		e := Provincia{Sigla: p.Sigla, Nome: p.Nome, Regione: byID[p.RegioneID]}
		province = append(province, e)
		provByKey[dictKey(p.Nome)] = e
		provByKey[dictKey(p.Sigla)] = e
	}
	sort.Slice(regioni, func(i, j int) bool { return placeKey(regioni[i]) < placeKey(regioni[j]) })
	sort.Slice(province, func(i, j int) bool { return placeKey(province[i].Nome) < placeKey(province[j].Nome) })
}

// dictKey ignores case, accents, spaces and punctuation ("forli cesena" =
// "Forlì-Cesena", "Friuli Venezia Giulia" = "Friuli-Venezia Giulia").
func dictKey(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, placeKey(s))
}

// NormalizeProvincia returns the dictionary name for a typed provincia
// (name or sigla: "vi", "VICENZA" → "Vicenza"); "" stays "".
func NormalizeProvincia(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if p, ok := provByKey[dictKey(v)]; ok {
		return p.Nome, nil
	}
	return "", fmt.Errorf("provincia sconosciuta: %q (scegli dall'elenco, nome o sigla)", v)
}

// NormalizeRegione returns the dictionary name for a typed regione.
func NormalizeRegione(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if r, ok := regByKey[dictKey(v)]; ok {
		return r, nil
	}
	return "", fmt.Errorf("regione sconosciuta: %q (scegli dall'elenco)", v)
}
