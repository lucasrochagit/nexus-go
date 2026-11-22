# Nexus Go — Decisões de design

> Status: **rascunho (v0)**, consolidado em 2026-11-22.
> O contrato de armazenamento (chaves, formato JSON, protocolo de escrita) está em
> [`SPEC.md`](../SPEC.md). Este documento registra **o que** foi decidido para a biblioteca e
> **por quê**.

## Objetivo

Biblioteca Go que entrega remote configs (booleanos, inteiros, floats, strings, objetos e
arrays JSON)
para APIs e aplicações, lidas de um provider (Redis na v1) e mantidas em memória.

**Fora do escopo da v1:** escrita de configs (feita por um painel separado), controle de
acesso, segredos, override de valores entre scopes, conceito de ambiente.

## Visão geral

```
           painel (futuro)                      aplicação
                │ grava (SPEC.md §6)                 │ GetBool / GetInt / Scope(...) / GetObject
                ▼                                    ▼
   ┌──────────── Redis ────────────┐           ┌──────── nexus (core) ────────┐
   │ nexus:{scope}:configs  (Hash) │◄──────────│ snapshot imutável por scope  │
   │ nexus:{scope}:version  (int)  │ Load      │ polling + Pub/Sub + refresh  │
   │ nexus:{scope}:changed  (chan) │ Versions  │ fallbacks, logs, validação   │
   └───────────────────────────────┘ Watch     └──────────────┬───────────────┘
                                                              │ Provider
                                             provider/redis · provider/file · provider/memory
```

## Exemplo de uso

```go
rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"}) // a aplicação é dona da conexão

rc, err := nexus.New(nexus.Options{
    Namespace: "checkout-api",
    Scopes:    []string{"global", "payments"},
    Provider:  redisprovider.New(rdb),
    Logger:    slog.Default(), // opcional; slog.Default() se omitido
})
if err != nil {
    log.Fatal(err) // opções inválidas: erro de programação
}

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
if err := rc.Start(ctx); err != nil {
    slog.Warn("remote config indisponível, usando fallbacks", "err", err) // ou Fatal: a aplicação decide
}
defer rc.Close()

newFlow := rc.GetBool("new_checkout_flow", false)
timeout := rc.GetInt("timeout_ms", 3000)
maintenance := rc.Scope("global").GetBool("maintenance", false)
limits := nexus.GetObject(rc.Scope("payments"), "limits", Limits{MaxAmount: 500})
```

## Decisões

### D1. Leitura a partir de um snapshot em memória

A biblioteca carrega as configs para memória e as mantém atualizadas em segundo plano.
`Get*` não faz I/O e não retorna `error`; se o Redis cair, vale o último valor conhecido.

**Por quê:** configs são lidas no caminho crítico (a cada requisição). Ir ao Redis a cada
leitura colocaria latência de rede e a disponibilidade do Redis em todo request, e obrigaria
`ctx`/`error` em toda chamada.

### D2. Biblioteca somente leitura; escrita pelo painel

A biblioteca não grava. O formato no Redis é, portanto, um **contrato entre dois sistemas**
e está documentado em `SPEC.md`. O leitor é defensivo: entradas inválidas são descartadas e
registradas, sem derrubar a aplicação nem o restante do scope.

**Por quê:** aplicações consumidoras não devem conseguir alterar configs de produção. Se o
painel for escrito em Go, um pacote leve de tipos e validação poderá ser compartilhado no
futuro.

### D3. Um Hash por scope

Cada scope é um Hash (`nexus:{scope}:configs`), com campo = nome da config.

**Por quê:** `HGETALL` carrega o scope inteiro de forma atômica e em uma única ida ao Redis.
Uma chave por config exigiria `SCAN` (lento, com várias idas ao Redis e não atômico).

### D4. Namespace obrigatório; outros scopes por acesso explícito, sem mesclar

- `Options.Namespace` (obrigatório) é o scope próprio da aplicação; `rc.Get*` lê dele.
- Outros scopes (`global`, `payments`, …) são declarados em `Options.Scopes` e acessados por
  `rc.Scope(nome)`, que retorna um handle com os mesmos métodos.
- **Não há override nem precedência entre scopes**: cada config tem um único dono. Se uma
  aplicação precisar de um valor diferente do compartilhado, ela cria a própria chave e passa
  a ler dela.
- `global` é só um nome de scope, combinado por convenção.
- Só os scopes declarados entram no snapshot.

**Por quê:** é explícito (o código mostra de onde vem cada valor) e evita regras de
precedência no painel. Um handle por scope não multiplica a API (`GetGlobalInt`, …) e não
depende de parâmetros variádicos frágeis.

**Nome:** o conceito se chama *scope*, e não *context*, para não confundir com
`context.Context`.

### D5. Sem controle de acesso na v1

Não há lista de permissões por scope. Qualquer aplicação com credencial do Redis consegue ler
qualquer scope.

**Por quê:** uma verificação feita pela biblioteca não seria segurança, porque a aplicação
acessa o Redis diretamente e poderia ignorá-la. **Segredos não devem ser guardados em
remote config.**

### D6. Um Redis por ambiente

A biblioteca não tem conceito de ambiente; cada ambiente aponta para seu próprio Redis.
`Options.Prefix` (padrão `nexus`) existe como saída para quem precisar compartilhar uma
instância.

### D7. Sincronização: versão por scope + Pub/Sub + refresh de segurança

- O painel faz `INCR nexus:{scope}:version` na mesma transação da escrita e depois
  `PUBLISH nexus:{scope}:changed` (`SPEC.md` §6).
- **Pub/Sub ligado por padrão**: ao receber um aviso, a biblioteca confere a versão do scope
  e recarrega se ela mudou.
- **Polling de versão sempre ativo** como rede de segurança: um único round-trip
  (pipeline de `GET`s) para todos os scopes, e `HGETALL` só nos scopes cuja versão mudou.
- **A cada (re)inscrição no canal**, a biblioteca confere a versão de todos os scopes,
  cobrindo avisos perdidos durante uma desconexão.
- **Refresh completo periódico** de todos os scopes, independente da versão, para o caso de
  alguém editar o Redis sem seguir o protocolo.
- **Jitter** antes de recarregar, para que todas as instâncias não rodem `HGETALL` ao mesmo
  tempo.
- Carga de um scope: `MULTI` / `GET version` / `HGETALL configs` / `EXEC`. As hash tags
  garantem o mesmo slot. Assim versão e dados são sempre consistentes entre si.

Intervalos padrão propostos (configuráveis em `Options`):

| Parâmetro | Padrão |
|---|---|
| Polling de versão com Pub/Sub ligado | 60 s |
| Polling de versão com Pub/Sub desligado | 15 s |
| Refresh completo de segurança | 10 min |
| Jitter máximo antes de recarregar | 1 s |

**Por quê:** Pub/Sub sozinho perde mensagens (reconexão, failover, deploy) e deixaria
instâncias com valores antigos indefinidamente. Polling sozinho atrasa a propagação.
Keyspace notifications foram descartadas: têm o mesmo problema do Pub/Sub e exigem
configuração do servidor, que muitas vezes fica desligada em Redis gerenciado.

### D8. `last_modified_at` é apenas metadado

O campo continua em cada config, mas não participa da sincronização. É exposto via
`Lookup` (D14) e exibido no painel.

**Por quê:** usá-lo para detectar mudança exigiria ler todas as configs. Além disso,
timestamps podem coincidir ou voltar no tempo, o que não acontece com um contador `INCR`.

### D9. Inicialização em duas etapas: `New` + `Start`

- `nexus.New(opts)` só valida as opções; não faz I/O. Erro aqui é erro de programação.
- `rc.Start(ctx)` faz a primeira carga dentro do prazo do `ctx` e inicia a sincronização.
  Se a carga falhar, retorna o erro, **mas a sincronização continua tentando** em segundo
  plano.
- Cada aplicação decide se o erro de `Start` é fatal.
- `rc.Close()` encerra o polling e o Pub/Sub da biblioteca; não fecha o client Redis.
- `rc.Ready() bool` indica se o snapshot inicial já foi carregado (para health checks).

**Por quê:** nem a política "nunca subir sem configs" nem a política "sempre subir" servem
a todas as aplicações. Com as duas etapas, essa escolha vira um `if` no código de quem usa,
sem precisar de flags.

### D10. Providers plugáveis; módulos separados para dependências externas

```go
type Provider interface {
    Versions(ctx context.Context, scopes []string) (map[string]int64, error)
    Load(ctx context.Context, scope string) (map[string]RawConfig, error)
}

// Opcional: providers capazes de avisar mudanças (ex.: Redis Pub/Sub).
type Notifier interface {
    Watch(ctx context.Context, scopes []string, changed func(scope string)) error
}
```

- O core (snapshot, fallbacks, logs, validação, polling) é independente do provider.
- Providers da v1:
  - `redis`: produção.
  - `file`: desenvolvimento local, um JSON por scope (`SPEC.md` §8).
  - `memory`: valores definidos em código, para testes unitários das aplicações.
- **Sem SQLite**: é difícil de editar à mão, e o driver exige cgo ou traz uma dependência
  grande.
- O core e os providers sem dependências externas (`file`, `memory`) ficam no módulo raiz.
  O provider `redis` fica em um **módulo Go separado** (`provider/redis/go.mod`), para que
  o core não dependa do `go-redis`.

### D11. Conexão Redis injetada

`redisprovider.New(client redis.UniversalClient)` recebe um client já configurado pela
aplicação. A biblioteca **não lê variáveis de ambiente** e não fecha o client. Um atalho
`FromURL` pode ser oferecido no futuro.

**Por quê:** a aplicação já controla TLS, pool, timeouts, autenticação e instrumentação.
`UniversalClient` cobre standalone, Sentinel e Cluster. Produção hoje usa standalone, mas as
chaves já usam hash tags para não exigir migração se um dia for Cluster.

### D12. API de leitura

`*Client` (namespace próprio) e `*Scope` implementam a mesma interface `Reader`:

```go
GetBool(key string, fallback bool) bool
GetInt(key string, fallback int) int
GetFloat(key string, fallback float64) float64
GetString(key string, fallback string) string
Lookup(key string) (Info, bool)
```

Objetos e arrays JSON usam uma função genérica do pacote, porque métodos em Go não podem ter
parâmetros de tipo:

```go
func GetObject[T any](r Reader, key string, fallback T) T

limits  := nexus.GetObject(rc, "limits", Limits{})                  // type: object, T inferido pelo fallback
methods := nexus.GetObject(rc, "payment_methods", []string{"pix"})  // type: array
rules   := nexus.GetObject[*Rules](rc, "rules", nil)                // T explícito quando o fallback é nil
```

Regras:

- `GetObject` aceita configs dos tipos `object` e `array`. Quem decide se o formato serve é a
  decodificação em `T`: uma falha ali é tratada como "type mismatch".
- `object` e `array` são tipos separados para que o tipo diga a verdade e o painel possa
  oferecer um editor e uma validação adequados a cada um. Atalhos como `GetStrings` não
  entram na v1.

- Se a chave não existir ou o tipo for incompatível, o `Get*` devolve o fallback e registra
  em log (D13).
- `GetInt` só aceita `integer`. `GetFloat` aceita `float` e `integer`, porque a conversão
  não perde informação.
- `GetObject` decodifica o JSON **a cada chamada**. Um cache do valor decodificado
  compartilharia maps e slices entre os chamadores, e uma aplicação que alterasse o objeto
  retornado corromperia o snapshot. Otimizar só se um benchmark mostrar necessidade.
- Antes do primeiro carregamento, `Get*` devolve o fallback e registra um único log
  "not ready", em vez de um "config not found" por chave.

### D13. Logs com `log/slog`

`Options.Logger *slog.Logger`; se omitido, usa `slog.Default()`. Todos os logs são
estruturados (`scope`, `key`, `err`, …).

| Evento | Nível | Frequência |
|---|---|---|
| scope not found (`Scope()` com scope não declarado em `Options.Scopes`) | `Error` | uma vez por scope |
| config not found | `Warn` | uma vez por chave, rearmado quando o scope recarrega |
| type mismatch | `Warn` | uma vez por chave, rearmado quando o scope recarrega |
| entrada inválida no provider | `Warn` | uma vez por carga do scope |
| falha de sincronização | `Warn` | com limite de frequência; `Info` ao se recuperar |
| not ready | `Warn` | uma vez até o primeiro carregamento |

`Scope()` com um nome não declarado devolve um handle que sempre retorna o fallback. Um scope
declarado que não existe no Redis é carregado vazio, e suas chaves geram "config not found".

### D14. Detalhes de implementação acordados

- **Concorrência:** cada scope tem um snapshot imutável, trocado com `atomic.Pointer`.
  Leituras não usam lock, e um scope nunca é exposto pela metade.
- **Falha parcial:** se um scope falhar ao carregar, ele mantém a última versão válida, e os
  demais scopes são atualizados normalmente.
- **Nomes:** scopes, chaves e prefix seguem `^[a-z0-9_-]+$`, validados no `New`.
- **Metadados:** `Lookup(key)` retorna
  `Info{Scope, Key, Type, Raw json.RawMessage, LastModifiedAt time.Time}`.
- **Versionamento:** a biblioteca fica em `v0.x` até o painel existir e o contrato ser
  validado de ponta a ponta.
- **Código do formato é interno:** o tipo do documento, a validação de valor × tipo, as
  regras de nome e a montagem dos nomes das chaves ficam em `internal/schema`. O painel será
  escrito em outra linguagem, então o contrato entre os dois é só a `SPEC.md`, cujos
  exemplos de validação (§4.1) servem de casos de teste para ambos. O pacote pode ser
  exportado depois sem quebrar ninguém.

### D15. Hook de eventos para métricas (`OnEvent`)

```go
type EventKind int

const (
    EventScopeLoaded     EventKind = iota // scope (re)carregado com sucesso
    EventScopeLoadFailed                  // falha ao carregar um scope (mantém a última versão boa)
    EventSyncFailed                       // falha no polling ou no Pub/Sub
    EventSyncRecovered                    // sincronização voltou a funcionar
    EventInvalidEntry                     // entrada descartada (JSON inválido, type desconhecido...)
    EventConfigNotFound                   // Get* caiu no fallback: chave inexistente
    EventTypeMismatch                     // Get* caiu no fallback: tipo incompatível
    EventScopeNotFound                    // Scope() com scope não declarado
    EventNotReady                         // Get* antes do primeiro carregamento
)

type Event struct {
    Kind     EventKind
    Scope    string
    Key      string        // eventos de leitura e InvalidEntry
    Version  int64         // ScopeLoaded
    Configs  int           // ScopeLoaded: quantas configs válidas
    Duration time.Duration // ScopeLoaded / ScopeLoadFailed: tempo da carga
    Err      error         // eventos de falha
}

nexus.Options{OnEvent: func(e nexus.Event) { /* Prometheus, OpenTelemetry, ... */ }}
```

- **Uma struct com `Kind`, e não um tipo por evento.** Novos campos e novos `Kind`s podem ser
  adicionados sem quebrar quem já usa, desde que o `switch` da aplicação tenha `default`. É
  o mesmo padrão do `slog.Record`.
- **Independente dos logs.** Os logs seguem os limites de frequência da D13. O hook recebe
  **toda** ocorrência, porque métricas precisam de contagem exata. Isso vale também para
  `ConfigNotFound` e `TypeMismatch`, emitidos a cada `Get*` que cai no fallback.
- **Leituras bem-sucedidas não geram evento**, para não disparar o hook a cada `Get*` no
  caminho crítico. Se fizer falta, vira um `Kind` novo.
- **Chamada síncrona**, na goroutine que originou o evento, incluindo a do request nos
  eventos de leitura. O hook **precisa ser rápido, não bloquear e ser seguro para uso
  concorrente**. Sem hook configurado, o custo é só um teste de `nil`.
- **Pânico no hook é recuperado** com `recover` e registrado em log. Um bug no código de
  métricas não pode derrubar o request nem a goroutine de sincronização.

**Por quê:** permite ligar a biblioteca em Prometheus, OpenTelemetry ou qualquer outro
sistema sem adicionar dependências ao core. OpenTelemetry embutido foi descartado porque
traria uma dependência grande ao core (D10).

## Questões em aberto

Nenhuma no momento.

Resolvidas:

- **Arrays** (2026-11-22): tipo `array` próprio, lido com `GetObject` (D12).
- **Durações** (2026-11-22): sem tipo `duration` e sem convenção obrigatória na v1. Quem
  cadastra a config decide o tipo e o nome. A recomendação de indicar a unidade no nome
  (`timeout_ms`, `cache_ttl_s`) está nas boas práticas do `README.md`. Um tipo `duration`
  pode ser adicionado depois sem quebrar nada, porque seria um tipo novo.
- **Métricas** (2026-11-22): hook `OnEvent` na v1 (D15).
- **Pacote compartilhado com o painel** (2026-11-22): o painel será escrito em outra
  linguagem. O código do formato fica em `internal/schema`, e o contrato é a `SPEC.md`,
  com exemplos de validação (§4.1) (D14).

## Próximos passos no repositório

- ~~Ajustar o module path para `github.com/lucasrochagit/nexus-go`~~ (feito).
- ~~Remover `viper` do `go.mod` e mover `go-redis` para o módulo `provider/redis`~~ (feito:
  o módulo raiz não tem dependências; `provider/redis` é o módulo
  `github.com/lucasrochagit/nexus-go/provider/redis`, pacote `redisprovider`).
- `.env` e `.env.example` são responsabilidade da aplicação, não da biblioteca. Mantê-los
  apenas em exemplos ou testes de integração, se fizer sentido.
- Implementar nesta ordem: core + provider `memory` → provider `file` → provider `redis`.
