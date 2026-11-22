# Nexus — Especificação do formato de armazenamento

> Status: **rascunho (v0)**. Este documento é o contrato entre quem **lê** as remote configs
> (a biblioteca `nexus-go`) e quem **grava** (o painel de administração, a ser construído).
> Qualquer sistema que grave configs no Redis precisa seguir esta especificação; caso
> contrário, as aplicações podem não perceber a mudança ou descartar a entrada como inválida.

As palavras **DEVE**, **NÃO DEVE**, **DEVERIA** e **PODE** seguem o sentido da RFC 2119.

## 1. Conceitos

| Termo | Significado |
|---|---|
| **config** | Um valor remoto identificado por uma chave (ex.: `new_checkout_flow`). |
| **scope** | Um conjunto nomeado de configs (ex.: `checkout-api`, `payments`, `global`). É a unidade de armazenamento, versionamento e notificação. |
| **namespace** | O scope próprio de uma aplicação, informado por ela na inicialização. Do ponto de vista do armazenamento, é um scope como qualquer outro. |
| **prefix** | Prefixo de todas as chaves do Nexus. Padrão: `nexus`. |

Cada ambiente (staging, produção…) usa uma **instância Redis própria**. O formato não tem
conceito de ambiente.

## 2. Regras de nomes

- Nomes de **scope** e de **config** DEVEM casar com `^[a-z0-9_-]+$`.
- Em particular, `:`, `{` e `}` são proibidos, pois fazem parte da estrutura das chaves.
- O **prefix** DEVE casar com `^[a-z0-9_-]+$`.

## 3. Chaves no Redis

Para cada scope `<scope>`, existem até três nomes. As chaves `{` `}` são **literais**: são
*hash tags* do Redis Cluster e garantem que as chaves de um mesmo scope caiam no mesmo slot,
permitindo transações também em Cluster (em Redis standalone elas não têm efeito).

| Nome | Tipo Redis | Conteúdo |
|---|---|---|
| `<prefix>:{<scope>}:configs` | Hash | campo = nome da config; valor = documento JSON (seção 4) |
| `<prefix>:{<scope>}:version` | String (inteiro) | contador de versão do scope (seção 5) |
| `<prefix>:{<scope>}:changed` | Canal Pub/Sub | aviso de mudança no scope (seção 6) |

Exemplo para o scope `checkout-api` com prefix padrão:

```
nexus:{checkout-api}:configs
nexus:{checkout-api}:version
nexus:{checkout-api}:changed
```

## 4. Documento de uma config

Cada valor do Hash DEVE ser um objeto JSON (UTF-8) com exatamente estes campos:

```json
{
  "type": "boolean",
  "value": false,
  "last_modified_at": "2026-11-22T18:09:00Z"
}
```

| Campo | Obrigatório | Descrição |
|---|---|---|
| `type` | sim | Um de `boolean`, `integer`, `float`, `string`, `object`, `array`. |
| `value` | sim | O valor, que DEVE ser compatível com `type` (tabela abaixo). |
| `last_modified_at` | sim | Instante da última alteração, em RFC 3339, em UTC (sufixo `Z`). **Apenas informativo**: não é usado para detectar mudanças. |

Compatibilidade entre `type` e `value`:

| `type` | `value` aceito |
|---|---|
| `boolean` | `true` ou `false` |
| `integer` | número JSON sem parte fracionária nem expoente, no intervalo de `int64` |
| `float` | número JSON finito |
| `string` | string JSON |
| `object` | objeto JSON (`{...}`) |
| `array` | array JSON (`[...]`) |

Campos desconhecidos DEVERIAM ser ignorados pelo leitor, para permitir evolução do formato.

### 4.1 Exemplos de validação

Casos que o leitor usa como referência. Um escritor DEVERIA rejeitar na gravação tudo o que
o leitor descartaria. Nos exemplos, `"…"` representa um `last_modified_at` válido.

| Documento | Resultado | Motivo |
|---|---|---|
| `{"type":"boolean","value":true,"last_modified_at":"2026-11-22T18:09:00Z"}` | válido | |
| `{"type":"boolean","value":"true","last_modified_at":"…"}` | inválido | string não é `boolean` |
| `{"type":"integer","value":3000,"last_modified_at":"…"}` | válido | |
| `{"type":"integer","value":3000.0,"last_modified_at":"…"}` | inválido | tem parte fracionária |
| `{"type":"integer","value":1e3,"last_modified_at":"…"}` | inválido | tem expoente |
| `{"type":"integer","value":9223372036854775808,"last_modified_at":"…"}` | inválido | fora do intervalo de `int64` |
| `{"type":"float","value":0.05,"last_modified_at":"…"}` | válido | |
| `{"type":"float","value":3,"last_modified_at":"…"}` | válido | inteiro é um número finito |
| `{"type":"string","value":"","last_modified_at":"…"}` | válido | string vazia é permitida |
| `{"type":"string","value":null,"last_modified_at":"…"}` | inválido | `null` não é aceito em nenhum tipo |
| `{"type":"object","value":{"max_items":10},"last_modified_at":"…"}` | válido | |
| `{"type":"object","value":[1,2],"last_modified_at":"…"}` | inválido | array não é `object` |
| `{"type":"array","value":["pix","card"],"last_modified_at":"…"}` | válido | |
| `{"type":"array","value":{},"last_modified_at":"…"}` | inválido | objeto não é `array` |
| `{"type":"duration","value":30,"last_modified_at":"…"}` | inválido | `type` desconhecido |
| `{"type":"boolean","value":true}` | inválido | `last_modified_at` ausente |
| `{"type":"boolean","value":true,"last_modified_at":"22/11/2026"}` | inválido | não é RFC 3339 |
| `{"type":"boolean","value":true,"last_modified_at":"…","owner":"time-x"}` | válido | campos desconhecidos são ignorados |
| `{"type":"boolean","value":tru` | inválido | JSON malformado |

## 5. Versão do scope

- `<prefix>:{<scope>}:version` é um contador inteiro incrementado com `INCR` a **cada**
  alteração em qualquer config do scope (criação, atualização ou remoção).
- Uma chave de versão inexistente equivale à versão `0`.
- A chave de versão **NÃO DEVE** ser removida, sobrescrita com `SET` nem decrementada — nem
  mesmo ao esvaziar ou "apagar" um scope. Recriá-la a partir de zero poderia reproduzir uma
  versão já vista por um leitor, que então não perceberia a mudança.
- Leitores comparam versões por **diferença**, não por "maior que".

## 6. Protocolo de escrita

### 6.1 Criar ou atualizar uma config

O escritor DEVE gravar o documento e incrementar a versão **na mesma transação**, e em
seguida publicar o aviso:

```
MULTI
HSET nexus:{checkout-api}:configs new_checkout_flow '{"type":"boolean","value":true,"last_modified_at":"2026-11-22T18:09:00Z"}'
INCR nexus:{checkout-api}:version
EXEC
PUBLISH nexus:{checkout-api}:changed 1
```

- O `PUBLISH` DEVE acontecer **depois** do `EXEC` bem-sucedido.
- O conteúdo da mensagem publicada é ignorado pelo leitor; o escritor PODE enviar a nova
  versão retornada pelo `INCR`, para fins de depuração.
- Alterações em várias configs do mesmo scope PODEM ser feitas numa única transação com um
  único `INCR` e um único `PUBLISH`.

### 6.2 Remover uma config

```
MULTI
HDEL nexus:{checkout-api}:configs new_checkout_flow
INCR nexus:{checkout-api}:version
EXEC
PUBLISH nexus:{checkout-api}:changed 1
```

### 6.3 Esvaziar um scope

```
MULTI
DEL  nexus:{checkout-api}:configs
INCR nexus:{checkout-api}:version
EXEC
PUBLISH nexus:{checkout-api}:changed 1
```

A chave de versão é mantida (seção 5).

### 6.4 Por que o protocolo importa

Os leitores mantêm um snapshot em memória e só recarregam um scope quando sua versão muda.
Uma escrita que não incremente a versão **não chega às aplicações** até o próximo refresh
completo de segurança (que é espaçado, na ordem de minutos). Uma escrita sem `PUBLISH` chega,
mas com o atraso do polling.

## 7. Comportamento do leitor (informativo)

Esta seção descreve o que a biblioteca faz; não é obrigação do escritor.

- Carrega um scope lendo versão e Hash na mesma transação
  (`MULTI` / `GET version` / `HGETALL configs` / `EXEC`).
- Detecta mudanças por polling da versão, por Pub/Sub no canal `changed` e por um refresh
  completo periódico de segurança.
- Uma entrada inválida (JSON malformado, `type` desconhecido, `value` incompatível com
  `type`) é **descartada** e registrada em log; as demais entradas do scope são carregadas
  normalmente.
- Se a carga de um scope falhar, o leitor mantém a última versão válida desse scope.

## 8. Provider `file` (desenvolvimento local)

O provider `file` usa o mesmo documento da seção 4. Cada scope é um arquivo
`<diretório>/<scope>.json` contendo um objeto cujas chaves são os nomes das configs:

```json
{
  "new_checkout_flow": { "type": "boolean", "value": true,  "last_modified_at": "2026-11-22T18:09:00Z" },
  "timeout_ms":        { "type": "integer", "value": 3000,  "last_modified_at": "2026-11-22T18:09:00Z" },
  "checkout_limits":   { "type": "object",  "value": { "max_items": 10, "max_amount": 500.0 }, "last_modified_at": "2026-11-22T18:09:00Z" },
  "payment_methods":   { "type": "array",   "value": ["pix", "card"], "last_modified_at": "2026-11-22T18:09:00Z" }
}
```

A versão do scope é derivada da data de modificação do arquivo; não há chave de versão nem
Pub/Sub.
