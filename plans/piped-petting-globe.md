# Проверка доступности Telegram для каждого эндпоинта (`-tg`)

## Контекст

Форк называется `warpscout-tg`, и вопрос, ради которого он существует, — не «работает ли туннель
вообще», а «работает ли через этот эндпоинт Telegram». Сейчас скан отвечает только на первое:
`SEEN AS`/`NODE` говорят, куда выходит трафик, `TUN PING`/`LOSS` — что туннель жив. Но выходной
узел WARP может резать конкретные сервисы (README сам описывает это для узла `DME`), и текущий
скан такой эндпоинт от здорового не отличает.

Добавляем per-endpoint проверку: сразу после того, как туннель до эндпоинта поднят и подтверждён
мета-запросом, дозваниваемся по TCP до дата-центров MTProto через этот же туннель. Результат —
колонка `TG` (RTT до ближайшего DC либо `blocked`), фильтр `-tg-only` и приоритет в сортировке,
чтобы `-best`/`-conf` сами выбирали эндпоинт с живым Telegram.

Решения, зафиксированные с пользователем:

- **Что проверяем**: TCP-дозвон на порт 443 к IP дата-центров MTProto. Это ровно тот путь, которым
  ходит клиент Telegram: без DNS и без SNI, ловит блокировки по IP/сети выхода.
- **Когда**: по флагу `-tg`, по умолчанию выключено — как `-tun-ping` и `-speed`.
- **Что с результатом**: колонка + фильтр `-tg-only` + эндпоинты с живым Telegram сортируются выше
  остальных, поэтому `-best` и `-conf` выбирают их без дополнительных флагов.

## Реализация

### 1. Сама проверка — новый файл `telegram.go`

```go
// Адреса зашиты намеренно: клиент Telegram дозванивается по ним напрямую, поэтому
// резолв проверял бы путь, которым сам Telegram не ходит.
var telegramDCs = []string{
    "149.154.175.50:443",  // DC1
    "149.154.167.51:443",  // DC2
    "149.154.175.100:443", // DC3
    "149.154.167.91:443",  // DC4
    "91.108.56.130:443",   // DC5
}
```

Две функции:

- `firstReachable(ctx, dial func(context.Context, string) (net.Conn, error), addrs []string, timeout time.Duration) (time.Duration, bool)` —
  дозванивается до всех адресов **параллельно** под общим дедлайном, побеждает первый ответивший,
  соединение сразу закрывается. Параллельно, а не по очереди: последовательный обход на
  заблокированном выходе стоил бы `len(addrs) × timeout` (10 секунд на эндпоинт при дефолтном
  `-t 2`), а так потолок — один `timeout`. `dial` параметром, чтобы логика первого-успеха
  тестировалась без сети.
- `(s *ipStack) telegramRTT(ctx, timeout)` — обёртка, подставляющая `s.tnet.DialContext`.

Важно: переиспользовать `s.client` нельзя — его транспорт в [tunnel.go:32-37](tunnel.go#L32-L37)
жёстко дозванивается в `metaDialAddr()` и адрес из запроса игнорирует. Поэтому дозвон идёт
напрямую через `tnet`, как это уже делает `resolveMetaAddr`.

`ponytail:` только IPv4-адреса DC. Туннель WARP несёт обе семьи независимо от `-6`, так что
v4-адреса достижимы и в v6-скане; v6-список добавить, если появится сеть, где v4 к DC не идёт.

### 2. Где вызывается

В замыкании `work` внутри `runScan` ([main.go:508-530](main.go#L508-L530)), сразу после успешного
`tunnelProbe`, рядом с существующим блоком `pingHost`/`outer.pingTo`:

```go
if opts.tg {
    r.tg, r.tgOK = tn.stack().telegramRTT(ctx, timeout)
}
```

Сигнатуры `tunnelProbe`/`probeEndpoint` не трогаем: после их возврата туннель воркера всё ещё
поднят и настроен на этот эндпоинт (пир меняется только следующим `handshake`), так что проверка
идёт по тому же живому туннелю. У `masqueTunnel` тот же `*ipStack`, поэтому MASQUE работает без
отдельной ветки. `find-junk`/`find-sni` (`wantMeta=false`) не задеты вовсе.

Стоимость: один параллельный залп SYN на эндпоинт; худший случай — `+timeout` на эндпоинт при
полной блокировке, делённый на `-jt` воркеров.

### 3. Протяжка результата

Ровно по образцу `speed`/`tunPing`, без новых параметров в сигнатурах:

- `endpointResult` ([report.go:50](report.go#L50)): `tg time.Duration`, `tgOK bool`, `tgSeen bool`
  (`tgSeen` отличает «не проверяли» от «заблокировано»).
- `foundMsg` ([tui.go:28](tui.go#L28)): те же `tg`, `tgOK`, — заполняется в `runScan` рядом с
  `exit`/`colo`.
- Видимость колонки: `anyTGChecked(results)` по образцу `anySpeed` ([report.go:91](report.go#L91)),
  но по флагу `tgSeen`, а не по успеху, — иначе колонка пропадала бы ровно в случае «везде
  заблокировано». Так `writeRows`, `writePicksTable`, `writeNodePicks`, `writeToFile` остаются с
  прежними сигнатурами, и тесты отчёта не переписываются целиком.
- В TUI: поле `scanModel.tg`, выставляется в `runWithUI` из `opts` там же, где уже ставятся
  `m.header`/`m.dropLists` ([main.go:566-571](main.go#L566-L571)) — `newScanModel` не меняем.
- Рендер: `tgStr(r)` → `latencyStr(tg)` при `tgOK`, `blocked` при `tgSeen && !tgOK`, иначе `-`;
  плюс `tgCells`/`tgHeaders`/`tgField` — копии `speedCells`/`speedHeaders`/`speedField`
  ([report.go:107-126](report.go#L107-L126)). Колонка встаёт после `LOSS`, перед `SPEED`.
- `writeHeader` ([report.go:292](report.go#L292)): строка легенды `# TG = TCP RTT to the nearest
  Telegram MTProto DC, measured inside the tunnel`.
- Живой фид `renderFeed` ([tui.go:474](tui.go#L474)): колонка `TG` шириной 9 после блока
  `TUN PING`/`LOSS`, `blocked` красится `st.warn`.
- `plainEmit` не трогаем — там печатаются только строки узлов, а данные для скриптов идут
  в файл отчёта и в `-best`.

### 4. Сортировка и фильтр

- `lessByLossRTT` ([report.go:253](report.go#L253)): первым критерием `if a.tgOK != b.tgOK { return a.tgOK }`.
  Без `-tg` оба поля `false`, поэтому порядок в старых сценариях не меняется ни на строку.
  То же в `lessLatency` ([tui.go:281](tui.go#L281)) для живого фида.
- `sortNote`/`bestNote` ([report.go:128-140](report.go#L128-L140)) получают второй параметр `tg bool`
  и при нём начинают строку с `Telegram first,` — иначе непонятно, почему порядок другой.
- `-tg-only`: фильтр через существующий `filterResults` ([report.go:222](report.go#L222)), новая
  запись в таблице `applyFilters` ([main.go:~330](main.go#L330)), учёт в `filtered(opts)` и ветка
  в `noEndpointMsg` («no endpoint reached Telegram»).

### 5. Флаги ([flags.go](flags.go))

- `options`: `tg bool`, `tgOnly bool`.
- `-tg` в `scanGroup` рядом с `-speed`; `-tg-only` в `outputGroup` рядом с `-exclude-node`.
- Регистрация в `setupScanFlags` — только у команды `scan`.
- В `applyCommonFlags`: `-tg-only` включает `-tg` (тот же приём, что `-tun-ping-count` →
  `-tun-ping`, [flags.go:346-352](flags.go#L346-L352)), чтобы фильтр не отсекал всё молча.

### 6. Тесты ([warp_test.go](warp_test.go))

- `TestFirstReachable` — подставной `dial`: медленный адрес + быстрый, побеждает быстрый; все
  падают → `ok=false`; общий дедлайн соблюдён. Логику первого-успеха проверяем без сети.
- `TestTelegramColumn` — `writeFullReport` показывает `TG` при `tgSeen` и не показывает без него;
  `blocked` для `tgSeen && !tgOK`.
- `TestLessByLossRTTTelegram` — рабочий Telegram сортируется выше при худшем ping; при
  `tgSeen=false` у обоих порядок прежний.
- `TestFilterTelegram` — `-tg-only` оставляет только `tgOK`.

### 7. Документация

- [README.md](README.md) и [README_RU.md](README_RU.md) — обе (конвенция репозитория): две строки
  в таблицу флагов `scan` (`-tg`, `-tg-only`), абзац в раздел про колонки о том, что именно
  проверяется (TCP до DC MTProto, без DNS и SNI) и что `-tg` меняет порядок сортировки.
- [CLAUDE.md](CLAUDE.md) — одна фраза в описание scan flow.

## Проверка

```sh
go build . && go test ./...
go test -run 'TestFirstReachable|TestTelegramColumn|TestLessByLossRTTTelegram|TestFilterTelegram' -v

go run . scan -n 2                 # без -tg вывод и порядок строк не изменились
go run . scan -n 2 -tg             # колонка TG в фиде, консольной таблице и файле отчёта
go run . scan -n 2 -tg -P          # TG соседствует с TUN PING/LOSS, ширины не разъезжаются
go run . scan -n 2 -tg-only -best  # печатает эндпоинт с живым Telegram (или падает с внятным текстом)
go run . scan -n 2 -tg -plain      # plain-режим не сломан
```

Отдельно проверить на сети, где Telegram недоступен напрямую (или временно подменив
`telegramDCs` на заведомо чёрную дыру вроде `10.255.255.1:443`), что колонка показывает `blocked`,
а скан не тормозит дольше, чем на один `-timeout` на эндпоинт.
