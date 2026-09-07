# Conceitos, em português

Escrito para reler antes de uma entrevista. Cada resposta usa este próprio
repositório como exemplo, porque exemplo abstrato não gruda.

---

## 1. Terraform: o erro de categoria

A confusão mais comum é tentar encaixar o Terraform numa categoria onde ele não
cabe.

| Categoria | Exemplos | Roda o tempo todo? | Vê requisição de usuário? |
| --- | --- | --- | --- |
| **Produto** — serve o usuário | o Go, o Angular, Postgres, Redis, load balancer | sim | sim |
| **Ferramenta** — prepara o produto | `npm`, `git`, `docker compose`, **Terraform** | não | nunca |

Terraform não é nada da primeira linha. Ele nunca vê uma requisição, nunca
guarda dado de usuário, não fica ligado. Roda, trabalha, morre:

```
$ pgrep -f tofu | wc -l     # antes
0
$ tofu plan                 # ~40 chamadas na API da AWS, e acabou
$ pgrep -f tofu | wc -l     # depois
0
```

### A analogia que resolve (está neste repo)

Abra o `docker-compose.yml`:

```yaml
services:
  postgres:
    image: postgres:16-alpine
  redis:
    image: redis:7-alpine
```

Você **descreve** o que quer, roda `docker compose up -d`, e o Compose cria os
containers e **sai**. Os containers continuam; o Compose não. Apague o Redis do
arquivo, rode de novo, e ele remove o container.

**Terraform é isso, mas para a AWS em vez do seu notebook.**

```
docker-compose.yml  →  docker compose up  →  containers no seu Mac
main.tf             →  terraform apply    →  recursos na AWS
```

Em Node: **`package.json` está para `node_modules` assim como Terraform está
para a AWS.** `npm install` não fica rodando junto com o app.

### Perguntas diretas

**"Ele gera um JSON?"** Gera o `terraform.tfstate`, que é JSON. Mas isso é o
**caderno de anotações** dele, não o produto. Equivale ao `package-lock.json`:
existe para lembrar o que foi criado. Ninguém usa `node_modules` como banco.

**"É um job runner que gerencia requisições?"** Não. Zero relação com
requisições — não tem fila, não tem worker, não recebe HTTP. Quem faz isso aqui
é o **SQS + `cmd/worker`**.

**"Não daria pra usar Redis para esses dados?"** Compare os tempos de vida:

- **Redis** guarda dado *enquanto o app roda* — milhares de leituras por segundo.
- **State do Terraform** é lido *duas vezes por semana*, quando alguém muda
  infraestrutura.

Seria como guardar o `package.json` no Redis. Não é errado, é que não existe
problema sendo resolvido.

### Por que não escrever um app em Node?

Você pode — e já fez, em bash, no `infra/localstack-init.sh`:

```bash
$AWS sqs create-queue --queue-name "$DLQ"
DLQ_ARN=$($AWS sqs get-queue-attributes ... )
$AWS sqs create-queue --queue-name "$QUEUE" --attributes "..."
```

Funciona. Agora faça esse script:

1. **Dizer o que vai mudar antes de mudar** — ler o estado atual e comparar campo a campo
2. **Perceber que alguém mexeu no console às 2h da manhã**
3. **Apagar** o que ele criou — o script não faz ideia do que criou
4. **Descobrir a ordem sozinho** — hoje a ordem é a que você digitou; trocar as linhas quebra

Implementou as quatro? Você reescreveu o Terraform, sozinho, com bugs, e sem os
milhares de providers prontos.

E o argumento decisivo: **isso não é feature do produto.** Não roda em produção,
não afeta usuário. Escrever em Node não te dá velocidade de time nem reuso.

> **Honestidade para entrevista:** existe o **Pulumi**, que é literalmente
> "Terraform em TypeScript/Go/Python". Então a resposta certa não é "não dá", é:
> *"dá, mas o Terraform já me entrega o motor de diff, o grafo de dependências e
> o ecossistema de graça — eu escrevo 40 linhas em vez de 4000"*.

A comparação que importa de verdade não é Terraform vs Node. É **Terraform vs
clicar no console**. É assim que todo mundo começa, e o problema aparece seis
meses depois: ninguém lembra o que clicou, staging não é igual a produção, e não
dá para revisar um clique num pull request.

---

## 2. Por que Go em vez de Node

Cuidado: **"Go é mais rápido" está errado** e um entrevistador bom percebe. Node
é rapidíssimo para I/O — foi feito para isso.

As razões reais, neste projeto:

**Concorrência de verdade.** O `cmd/worker` decide 8 disputas ao mesmo tempo,
cada uma com timeout próprio, tudo cancelável de uma vez:

```go
group.SetLimit(8)
for _, id := range ids {
    group.Go(func() error { p.handle(groupCtx, id); return nil })
}
```

Em Node, JavaScript roda em **uma thread só**. Para I/O paralelo, `Promise.all`
resolve. Mas coordenar workers com cancelamento, timeout e shutdown gracioso
vira `worker_threads` e fica desconfortável.

**Binário estático.** Vira um arquivo só: container de ~20MB, sem `node_modules`,
sem runtime. Isso importa no Fargate — sobe mais rápido, custa menos, e permite
`readonlyRootFilesystem` (veja `infra/terraform/ecs.tf`).

**Memória previsível.** Um processo de pé por semanas segurando conexões gasta
menos e varia menos. Menos RAM por task = conta menor.

**Mas seja honesto:** o `cmd/ingest` — que recebe um POST, valida HMAC e escreve
no banco — funcionaria perfeitamente em Node, sem diferença perceptível.

> **Resposta pronta:** *"Go pelo modelo de concorrência e pelo binário estático,
> não por velocidade bruta. O worker que drena a fila de deadlines é
> concorrência estruturada com cancelamento — é onde Go brilha. Um serviço HTTP
> que é I/O puro, Node faz igual. Por isso a vaga pede os dois: são partes
> diferentes do sistema."*

---

## 3. HMAC

**H**ash-based **M**essage **A**uthentication **C**ode. Uma assinatura que prova
duas coisas ao mesmo tempo:

1. **A mensagem não foi alterada** no caminho
2. **Quem enviou conhece o segredo compartilhado**

O segundo ponto é o que diferencia de um hash comum. Qualquer um calcula
`sha256(mensagem)`. Só quem tem a chave calcula `HMAC(chave, mensagem)`.

Aqui, o processador e a plataforma compartilham `whsec_...`. O emissor calcula
sobre `"<timestamp>.<corpo>"` e manda no header:

```
X-Processor-Signature: t=1788635675,v1=654f06c856baf080...
```

O `cmd/ingest` recalcula com o segredo do merchant e compara.

Dois detalhes do código que existem por motivo:

- **O timestamp entra no que é assinado.** Sem isso, quem capturou uma
  requisição válida poderia reenviá-la para sempre. Com isso, qualquer coisa
  fora da janela de 5 minutos é rejeitada.
- **`hmac.Equal` em vez de `==`.** Comparação byte a byte retorna mais rápido
  quando o primeiro byte está errado do que quando o último está. Isso vaza
  informação suficiente para descobrir a assinatura correta, um caractere por
  vez — um *timing attack*.

E a parte que permite rotacionar a chave: `signing.VerifyAny` aceita um
**conjunto** de segredos. Rotação não é instantânea — a chave nova é publicada,
os emissores levam minutos para adotar, e entregas assinadas com a antiga
continuam chegando. Aceitar só uma chave força um corte que rejeita tudo que
está em voo, e é por isso que chave que só rotaciona com downtime nunca é
rotacionada.

---

## 4. Processo longo vs serverless (e cold start)

**Os serviços deste projeto não funcionam como cloud functions.** Eles ficam de
pé o tempo todo.

Go é uma **linguagem**, não um modelo de deploy. Dá para rodar Go como processo
longo *ou* como Lambda. Node também. São escolhas independentes.

| | Processo longo (este projeto) | Serverless (Lambda) |
| --- | --- | --- |
| Quando inicia | uma vez, no deploy | quando chega requisição |
| Quando morre | no próximo deploy | depois de ficar ocioso |
| Cold start | só no deploy ou scale | possível a **cada** requisição |
| Custo | por hora de container | por invocação |

O `cmd/ingest` chama `server.ListenAndServe()` e **bloqueia para sempre**. O
`cmd/worker` roda um loop infinito. No `ecs.tf`, `desired_count = 2` significa
"mantenha 2 cópias vivas 24h por dia".

### Onde cold start realmente aparece aqui

Não por requisição — quando uma **task nova** sobe: deploy ou autoscaling. Por
isso existem `health_check_grace_period_seconds = 60` no `ecs.tf` e
`deregistration_delay` no `alb.tf`: enquanto a nova aquece, as antigas atendem.

E existe um efeito "frio" **medido neste projeto**:

```
primeira requisição:   245 ms
requisições seguintes:   3-7 ms
```

Não era container frio — o processo já estava rodando. Era o **pool de conexões
do Postgres**, que abre conexões sob demanda: a primeira requisição pagou
handshake TCP, TLS e autenticação; as seguintes reusaram a conexão.

Vale contar numa entrevista, porque mostra que você **mede em vez de supor**.

> **Quando serverless seria a escolha certa aqui?** O `cmd/ingest` é um
> candidato razoável: recebe um POST, valida, escreve, acabou. O que pesa contra
> é que webhook de pagamento é sensível a latência e cold start de Lambda
> adiciona centenas de milissegundos no pior caso. Já o `cmd/worker` **não** é
> candidato: ele mantém estado em memória, faz long polling de 20 segundos e
> precisa de shutdown gracioso — três coisas que brigam com o modelo.

---

## 5. As frases para levar

**Terraform**

1. *"Terraform não é um serviço, é uma ferramenta de linha de comando — como
   `docker compose`, mas para a nuvem em vez do meu notebook. Roda, chama a API
   da AWS, e morre."*
2. *"O valor é o `plan`: eu vejo exatamente o que vai mudar antes de mudar. É
   isso que permite revisar infraestrutura num pull request. O resto é
   consequência."*
3. *"O custo é o state file — guarda segredo em texto puro e é uma
   responsabilidade real. Vale citar em vez de fingir que não existe."*

**Go vs Node**

4. *"Go pelo modelo de concorrência e pelo binário estático, não por velocidade
   bruta."*

**Segurança**

5. *"HMAC prova integridade e autenticidade de uma vez. O timestamp entra na
   assinatura para impedir replay, e a comparação é constant-time para não
   vazar a assinatura por timing."*

**Operação**

6. *"Não é serverless: são processos longos no Fargate. Cold start aqui aparece
   no deploy, não por requisição — e o efeito frio que eu de fato medi foi o
   pool de conexões, 245ms na primeira e 3ms nas seguintes."*
