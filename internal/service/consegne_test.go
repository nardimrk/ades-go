package service

import (
	"sort"
	"testing"
)

func TestGroupByArea(t *testing.T) {
	cs := []CustomerDelivery{
		{Cliente: "Zeno", Provincia: "Vicenza", Citta: "Arzignano", Bottiglie: 6},
		{Cliente: "Anna", Provincia: "", Citta: "", Bottiglie: 1},
		{Cliente: "Bruno", Provincia: "verona", Citta: "", Bottiglie: 2},
		{Cliente: "Carla", Provincia: "Vicenza", Citta: "Chiampo", Bottiglie: 3},
		{Cliente: "Dario", Provincia: "Vicenza", Citta: "arzignano", Bottiglie: 4},
		{Cliente: "Elsa", Provincia: "Verona", Citta: "Bussolengo", Bottiglie: 5},
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if c := compareArea(a.Provincia, a.Citta, b.Provincia, b.Citta); c != 0 {
			return c < 0
		}
		return a.Cliente < b.Cliente
	})
	areas := groupByArea(cs)
	type city struct {
		name    string
		clients []string
	}
	var got [][]city
	var provs []string
	for _, a := range areas {
		provs = append(provs, a.Provincia)
		var cities []city
		for _, c := range a.Cities {
			var names []string
			for _, cd := range c.Customers {
				names = append(names, cd.Cliente)
			}
			cities = append(cities, city{c.Citta, names})
		}
		got = append(got, cities)
	}
	// provincia A-Z (case-insensitive), the unknown last; same for città
	if len(provs) != 3 || provs[0] != "verona" && provs[0] != "Verona" || provs[1] != "Vicenza" || provs[2] != "" {
		t.Fatalf("provinces = %q", provs)
	}
	if len(got[0]) != 2 || got[0][0].name != "Bussolengo" || got[0][1].name != "" {
		t.Fatalf("Verona cities = %+v", got[0])
	}
	if len(got[1]) != 2 || got[1][0].clients[0] != "Dario" || got[1][0].clients[1] != "Zeno" || got[1][1].name != "Chiampo" {
		t.Fatalf("Vicenza cities = %+v", got[1])
	}
	if areas[1].Clienti != 3 || areas[1].Bottiglie != 13 {
		t.Fatalf("Vicenza totals = %d clienti, %d bott.", areas[1].Clienti, areas[1].Bottiglie)
	}
}
