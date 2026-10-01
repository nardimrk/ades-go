# ADES Wine Club — Go

Riscrittura di `ades` in un **singolo binario Go**:

```
WhatsApp ──(multi-device)──▶ whatsmeow ──▶ collector ──▶ SQLite ──▶ servizi ──▶ HTTP + Templ + HTMX ──▶ Browser
```

| Prima (`ades`)                                 | Ora (`adesgo`)                                  |
|------------------------------------------------|-------------------------------------------------|
| `wa-service` Node + whatsapp-web.js + Chromium | `internal/wa` — whatsmeow (protocollo nativo)   |
| `collector.py` (socket.io)                     | `internal/collector` — eventi live              |
| `importer` ogni 5 min (`import_history.py`)    | history sync di WhatsApp + "Richiedi storico"   |
| `sync_contacts.py`                             | sync nomi contatti ogni 5 min (store whatsmeow) |
| `backup_db.py` (container a parte)             | backup settimanale integrato                    |
| dashboard Streamlit                            | `internal/web` — Templ + HTMX                   |
| 5 container Docker                             | 1 container (o il solo binario)                 |

SQLite usa `modernc.org/sqlite` (Go puro, niente CGO). Le tabelle dell'app sono **le stesse di
`wine.db`**: il database esistente si riusa così com'è. Le tabelle di sessione di whatsmeow
(`whatsmeow_*`) stanno nello stesso file.

## Avvio

```bash
cp .env.example .env              # compila CHANNEL_ID, OWNER_ID, AUTH_EMAILS, APP_URL, …
mkdir -p data
cp ../ades/db/wine.db data/wine.db # per riusare i dati esistenti (facoltativo)

make run                          # oppure: docker compose up -d --build
```

1. Apri `APP_URL/login`, inserisci un'email presente in `AUTH_EMAILS`. Senza Mailgun il magic
   link viene stampato nei log. I vecchi link di accesso continuano a funzionare.
2. Vai su **WhatsApp** (`/whatsapp`) e inquadra il QR dal telefono
   (*Dispositivi collegati → Collega un dispositivo*). Il QR compare anche nel terminale.
   La sessione resta nel DB: serve farlo una volta sola.
3. Nella stessa pagina trovi l'elenco dei gruppi con il loro id, da usare come `CHANNEL_ID`.

Al primo collegamento WhatsApp invia la cronologia recente, che viene classificata e importata
automaticamente. Per andare più indietro c'è **Richiedi storico** (il telefono deve essere online)
oppure **Importa Chat** con l'export `.txt`.

## Variabili d'ambiente

| Variabile | Obbligatoria | Descrizione |
|---|---|---|
| `CHANNEL_ID` | sì | Id dei gruppi da monitorare, separati da virgola |
| `OWNER_ID` | no | Autore delle inserzioni (`…@c.us` o `…@lid`); vuoto = messaggi inviati dall'account collegato |
| `AUTH_EMAILS` | sì | Email autorizzate alla dashboard |
| `APP_URL` | sì | URL pubblico (per i magic link) |
| `OWNER_DISPLAY_NAME` | no | Nome del venditore negli export `.txt` |
| `MAILGUN_API_KEY`, `MAILGUN_DOMAIN` | no | Invio magic link e backup |
| `BACKUP_TO` | no | Destinatario del backup settimanale (serve anche Mailgun) |
| `OPENROUTER_API_KEY`, `OPENROUTER_MODEL` | no | LLM per parsing e casi ambigui (altrimenti solo regole) |
| `DB_PATH` | no | Default `data/wine.db` |
| `HTTP_ADDR` | no | Default `:8080` |
| `WA_DISABLED` | no | `1` = non collegarsi a WhatsApp (istanze di test su una copia del DB) |

Un file `.env` nella directory corrente viene letto automaticamente.

## Struttura

```
cmd/adesgo/            main: wiring e shutdown
internal/config/       env + .env
internal/db/           schema/migrazioni, query del collector, backup
internal/textutil/     euristiche: difflib, campaign key, opzioni A./B., codici "3A", export .txt
internal/llm/          OpenRouter (con fallback tra modelli gratuiti)
internal/classify/     create / update / reply + modifiche d'ordine in linguaggio naturale
internal/collector/    messaggio → inserzione o risposta
internal/wa/           whatsmeow: pairing, eventi, history sync, gruppi, contatti
internal/service/      logica delle pagine (campagne, preventivi, consegne, …)
internal/web/          handler HTTP, auth, viste templ (views/)
static/                CSS, htmx, piccolo JS (embedded nel binario)
```

## Sviluppo

```bash
make generate   # rigenera i *_templ.go dopo aver modificato i .templ
make test
```

La UI deve restare usabile da telefono, tablet e desktop: le regole (tabelle che su
telefono diventano schede, `title-cell`, `wide`, HTMX, sicurezza dei dati, verifica con
screenshot) sono nella skill [`.claude/skills/adesgo-ui`](.claude/skills/adesgo-ui/SKILL.md).
`tools/shot` fa screenshot con emulazione dispositivo:

```bash
cd tools/shot && go run . "http://localhost:8080/inserzioni?token=…" /tmp/telefono.png 390 844
```

Le euristiche di `internal/textutil` sono port fedeli del codice Python e sono state verificate
sul `wine.db` reale (996 inserzioni, 8.685 risposte) con risultati identici: chiavi campagna,
opzioni, codici d'ordine, parsing e ratio di `difflib.SequenceMatcher`. Le regex Python usano
`\s`/`\w` Unicode, quindi le regex portate passano da `pyRe`.

## Differenze rispetto alla versione Python

- **Id dei messaggi**: stesso formato del bridge precedente (`false_<chat>_<id>_<autore>`, autori
  `@lid`/`@c.us`), così deduplica e citazioni funzionano anche sui dati storici.
- **Preventivi → Clienti**: un `manual_client_name` vuoto non conta più come cliente (in pandas
  `NaN` era considerato vero, quindi il conteggio era +1).
- **Messaggi doppi nello stesso secondo** (id sintetico + id reale, es. dopo una modifica): a parità
  di codici prevale la versione con l'id WhatsApp reale. Prima la scelta dipendeva da un sort non
  stabile di pandas.
- **Ordine salvato**: il dettaglio mostra le righe salvate dell'ordine, non le selezioni live.
- Nuovi prodotti creati da "Crea Preventivo" ricevono subito un codice `ITMnnnn`.
- Il backup via email **non include** le tabelle di sessione WhatsApp (contengono le chiavi del
  dispositivo collegato).
- I messaggi senza testo (media senza didascalia, reazioni, modifiche) non vengono salvati.
- Gli script una tantum (`migrate_users.py`, `backfill_campaign_merge.py`) non sono stati portati:
  sul DB attuale sono già stati applicati.
