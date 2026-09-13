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

### Provisionar não é popular

Outra confusão comum: achar que o Terraform "sobe dados para a AWS". Ele nunca
toca em dado. Dá para provar no próprio código — não existe um único recurso
aqui que escreva conteúdo:

```
$ grep -rn 'aws_s3_object' *.tf        # nenhum
$ grep -rn 'send_message' *.tf         # nenhum
```

Ele cria o bucket **vazio** e a fila **vazia**. É a mesma relação do
`docker-compose.yml` com o Postgres: o Compose cria o container, e quem insere
linha é `make migrate` e `make seed`. Outra ferramenta, outro momento.

| Recurso | O Terraform cria | Quem coloca coisa dentro |
| --- | --- | --- |
| Fila SQS | a fila, vazia | `cmd/ingest` → outbox → relay |
| Dead-letter queue | a fila, vazia | a própria política de redrive da AWS |
| Bucket S3 | o bucket, vazio | o **navegador**, com política assinada |
| Secrets Manager | a entrada, com placeholder | um processo separado, fora do Terraform |
| Cluster ECS | o cluster, vazio | as imagens que o pipeline empurra para o ECR |
| Postgres e Redis | nem isso — são só referências | migrations e a aplicação |

A única exceção no código é o `aws_secretsmanager_secret_version`, e ela grava
um placeholder **de propósito**, com `ignore_changes = [secret_string]` — ou
seja, existe justamente para o Terraform *não* gerenciar o conteúdo. Credencial
em arquivo `.tf` vai parar no git; credencial no state vai parar no bucket de
state em texto puro.

Terraform constrói o galpão, instala as prateleiras, passa a fiação e distribui
as chaves. Nunca põe uma caixa na prateleira.

### O teste que separa infraestrutura de dado

Pergunte: *"se eu rodar `terraform destroy` e depois `apply`, o que eu perco?"*

- **A infraestrutura volta idêntica** — filas, bucket, roles, cluster.
- **Os dados não voltam** — disputas, mensagens, arquivos de evidência. O
  Terraform nunca soube que existiam.

É exatamente por isso que `aws_s3_bucket_versioning` está ligado e que o load
balancer tem `enable_deletion_protection` em produção: o Terraform pode destruir
o recipiente que guarda um dado que ele não sabe recriar.

> **Resposta pronta:** *"Terraform provisiona, não popula. Cria a fila vazia, o
> bucket vazio e as roles de IAM que dizem o que cada serviço pode fazer. Quem
> publica mensagem é a aplicação; quem sobe arquivo é o navegador com uma
> política assinada. A linha divisória é: se `destroy` seguido de `apply` recria
> aquilo, é infraestrutura; se não recria, é dado."*

### E por que ele existe *neste* projeto, especificamente

Não é para "conectar na AWS" — quem conecta é o SDK dentro do Go, lendo
credenciais. São três motivos concretos:

1. **Os três binários Go precisam de um lugar para rodar.** Sem o `ecs.tf`, o
   `cmd/ingest` é um arquivo no seu disco.
2. **Sem IAM, eles não podem fazer nada.** O motivo mais importante e o menos
   óbvio. O worker só lê da fila porque o `iam.tf` permite — e repare que ele
   **não** tem `sqs:SendMessage`: consome e decide, não publica na fila que
   drena. Isso não é configuração de conexão, é uma decisão de segurança escrita
   em código e revisável num pull request.
3. **São 60 recursos.** Criados clicando no console, ninguém lembra o que foi
   clicado, staging não fica igual a produção, e não dá para revisar um clique.

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

## 2. Todo app precisa de Terraform?

**Não.** E "boa prática" virou desculpa para colocar Terraform em coisa que não
precisa.

### O teste de três perguntas

Se as três respostas forem não, **pule**:

1. Existe **mais de um ambiente** que precisa ser igual? (staging e produção)
2. **Mais de uma pessoa** mexe na infraestrutura?
3. Recriar tudo na mão levaria **mais de um dia**?

Nenhuma das perguntas é sobre o tamanho da aplicação. É sobre **ambientes,
pessoas e custo de recriar**.

### Quando NÃO precisa

| Situação | Por quê | Use isso |
| --- | --- | --- |
| Site estático ou SPA na Vercel, Netlify, Cloudflare Pages | A plataforma *é* a infra: conecta um repo e pronto. São três configurações. | O painel deles, ou `vercel.json` |
| App inteiro num PaaS — Railway, Render, Fly.io, Heroku | O ponto do PaaS é que infra é um arquivo de config. Terraform em cima é gerenciar os dois. | `fly.toml`, `render.yaml` |
| Um VPS só, com docker-compose | A infra é uma máquina. | Ansible, ou um script |
| Protótipo onde você ainda não sabe o formato | Terraform recompensa quem já sabe o que quer. | Console, e importe depois |
| Serverless com framework — SST, Serverless, SAM, Wrangler | Já geram a infra a partir do código e ficam no loop do desenvolvedor. | O próprio framework |
| Banco gerenciado — Supabase, Neon, PlanetScale | Clica uma vez, recebe uma URL. | Nada |
| Projeto solo, um ambiente | O ganho do `plan` é a revisão. Sem PR, sem revisão. | Nada |

O caso mais claro: **um blog em Next.js na Vercel.** Terraform ali seria um state
file para gerenciar um domínio e duas variáveis de ambiente — você adicionou uma
ferramenta, um arquivo que guarda segredo e um passo no deploy, para resolver
problema nenhum.

### Quando SIM

| Situação | Por quê |
| --- | --- |
| Staging precisa ser igual a produção | O momento em que divergem em silêncio é o momento em que você paga |
| Mais de uma pessoa mexe | O `plan` num pull request *é* o produto |
| Auditoria ou compliance | "Quem mudou esse security group e quando" vira `git log` |
| Ambientes efêmeros por PR | `destroy` confiável é a killer feature — sem ele você vaza recurso e dinheiro |
| Múltiplos provedores — AWS, Cloudflare, Datadog, GitHub | Aqui praticamente não tem concorrente |
| Recuperação de desastre | "Reconstruir a região" deixa de ser heroísmo e vira um comando |
| IAM não trivial | Permissão é decisão de segurança; merece revisão, não um clique |

O caso mais claro do outro lado: **uma fintech com staging e produção, cinco
engenheiros, e políticas de IAM que definem quem pode mover dinheiro.** Aqui
*não* ter Terraform é o erro.

### A escada, do mais leve ao mais pesado

```
nada / console          →  1 ambiente, solo, protótipo
arquivo da plataforma   →  PaaS (fly.toml, vercel.json)
docker-compose          →  uma máquina
CDK / SAM / SST         →  só AWS, time de TypeScript, muito serverless
Terraform / OpenTofu    →  multi-ambiente, multi-provedor, time
Terraform + Atlantis    →  time grande, compliance, aprovação obrigatória
```

Suba um degrau **quando doer**, não antes.

### E este projeto, sendo honesto

O `dispute-router` tem **um ambiente e uma pessoa**. Pelo teste acima, ele **não
precisa** de Terraform. Ele tem por dois motivos, e vale saber separá-los:

1. **Legítimo:** são 60 recursos com IAM por serviço. Criar clicando e depois
   lembrar do que foi clicado é inviável.
2. **Honesto:** o objetivo é aprender e conseguir falar sobre isso.

Se fosse um projeto real nesse estágio — uma pessoa, um ambiente — a escolha
defensável seria começar no console ou no CDK e adotar Terraform quando o
segundo ambiente aparecesse.

### O detalhe que tira a pressão

Dá para adotar depois. `terraform import` traz recursos existentes para o state:

```bash
terraform import aws_sqs_queue.events https://sqs.../disputes-events
```

É trabalhoso e manual, mas existe. Então "começar clicando e adotar quando doer"
é estratégia legítima — e provavelmente o padrão certo.

---

## 3. Os 60 recursos deste projeto, por grupo

O que o `tofu plan` cria. Vale olhar a proporção antes dos detalhes.

### Onde o código roda — ECS (8)

| Quantos | O quê | Para quê |
| --- | --- | --- |
| 1 | `aws_ecs_cluster` | Onde as tasks vivem. No Fargate é quase um namespace. |
| 1 | `aws_ecs_cluster_capacity_providers` | Deixa `FARGATE` e `FARGATE_SPOT` disponíveis |
| 3 | `aws_ecs_task_definition` | A receita **imutável** de cada container: imagem, cpu, memória, variáveis, roles |
| 3 | `aws_ecs_service` | Mantém N cópias vivas apontando para uma revisão |

Deploy é o *service* apontando para uma revisão nova. Rollback é apontar para a
anterior — que nunca foi apagada. Por isso rollback no ECS é um minuto, não um
build.

### De onde vem a imagem — ECR (6)

| Quantos | O quê | Para quê |
| --- | --- | --- |
| 3 | `aws_ecr_repository` | Um registro por serviço, com tag **imutável** |
| 3 | `aws_ecr_lifecycle_policy` | Guarda as últimas 30 imagens |

Tag imutável é o que faz um git sha significar algo: sem isso alguém empurra uma
imagem diferente sob a mesma tag e a versão que você acha que está rodando não é
a que está. E registro cresce para sempre e é cobrado por gigabyte.

### Quem pode fazer o quê — IAM (12)

| Quantos | O quê | Para quê |
| --- | --- | --- |
| 1 | `aws_iam_role` execution | Usada pelo **agente do ECS** antes do container subir |
| 3 | `aws_iam_role` task | Assumida pelo **seu processo**, em tempo de execução |
| 1 | `aws_iam_role_policy` | Deixa a execution role ler o secret injetado |
| 3 | `aws_iam_role_policy` | Uma por serviço: ingest, worker, api |
| 3 | `aws_iam_role_policy` | `ecs-exec`, para abrir shell num container rodando |
| 1 | `aws_iam_role_policy_attachment` | A policy gerenciada da AWS: puxar imagem, escrever log |

É o maior grupo depois da rede, e é o mais importante. Sem ele os binários sobem
e não conseguem fazer nada.

### Como o tráfego entra — rede (13)

| Quantos | O quê | Para quê |
| --- | --- | --- |
| 1 | `aws_lb` | O load balancer |
| 1 | `aws_lb_listener` | Porta 443, TLS |
| 2 | `aws_lb_listener_rule` | `/webhooks/*` vai para o ingest, `/api/*` para a api |
| 2 | `aws_lb_target_group` | Um para ingest, um para api — **o worker não tem** |
| 2 | `aws_security_group` | Um para o load balancer, um para as tasks |
| 3 | `aws_vpc_security_group_ingress_rule` | 443 do mundo no LB, e o LB alcançando ingest e api |
| 2 | `aws_vpc_security_group_egress_rule` | LB até as tasks, tasks até a AWS |

O worker não aparecer aqui **não é omissão, é o desenho**: nada roteia até ele,
então nada pode alcançá-lo.

### A fila — SQS (3)

| Quantos | O quê | Para quê |
| --- | --- | --- |
| 2 | `aws_sqs_queue` | A fila principal e a dead-letter |
| 1 | `aws_sqs_queue_redrive_allow_policy` | Permite mover mensagens **de volta** da DLQ |

Sem o terceiro, o `cmd/dlq replay` é um erro de permissão exatamente na hora em
que alguém precisa que funcione.

### O bucket de evidências — S3 (6)

| Quantos | O quê |
| --- | --- |
| 1 | `aws_s3_bucket` — o bucket |
| 1 | `aws_s3_bucket_public_access_block` — o que separa "tivemos um vazamento" de "tivemos um bucket" |
| 1 | `aws_s3_bucket_server_side_encryption_configuration` |
| 1 | `aws_s3_bucket_versioning` — evidência apagada é o que você não pode perder |
| 1 | `aws_s3_bucket_cors_configuration` — o navegador posta direto aqui |
| 1 | `aws_s3_bucket_lifecycle_configuration` — expira versões antigas e uploads abandonados |

Um bucket vira seis recursos porque o provider v5 separou cada aspecto. Isso é
bom: cada um aparece sozinho no diff, então "alguém desligou o bloqueio de acesso
público" é uma linha vermelha no `plan`.

### Os segredos (2)

| Quantos | O quê |
| --- | --- |
| 1 | `aws_secretsmanager_secret` — o recipiente |
| 1 | `aws_secretsmanager_secret_version` — um **placeholder**, com `ignore_changes` |

O conteúdo é gerenciado fora de propósito. Ver a seção 1.

### Observabilidade (8)

| Quantos | O quê | Para quê |
| --- | --- | --- |
| 3 | `aws_cloudwatch_log_group` | Um por serviço, com retenção — o CloudWatch guarda para sempre e cobra por isso |
| 1 | `aws_cloudwatch_metric_alarm` | Qualquer coisa na DLQ. Não existe número saudável acima de zero |
| 1 | `aws_cloudwatch_metric_alarm` | **Idade** da mensagem mais velha — a que mapeia para prazo perdido |
| 3 | `aws_cloudwatch_metric_alarm` | Um por serviço: rodando menos tasks do que deveria |

O teste de um alarme é se alguém consegue fazer algo às três da manhã. CPU alta
normalmente falha nesse teste; fila que não drena, não.

### Autoscaling (2)

| Quantos | O quê |
| --- | --- |
| 1 | `aws_appautoscaling_target` — só o worker escala |
| 1 | `aws_appautoscaling_policy` — por **profundidade da fila**, não por CPU |

CPU é um proxy para carga; a quantidade de mensagens esperando **é** a carga. Um
worker travado num banco lento está ocioso e atrasado ao mesmo tempo, e escalar
por CPU lê isso como "não tem nada para fazer".

### A proporção é o ponto

```
 8 recursos rodam o código
52 recursos são permissão, tráfego, e saber o que aconteceu
```

Três binários Go precisam de **oito** recursos para existir. Os outros cinquenta
e dois respondem "quem pode fazer o quê", "por onde entra o tráfego" e "como eu
descubro que quebrou".

É a resposta mais honesta para "por que infraestrutura é tanto trabalho": rodar o
código é a parte fácil.

---

## 4. Por que Go em vez de Node

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

## 5. HMAC

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

## 6. Processo longo vs serverless (e cold start)

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

## 7. Onde cada tecnologia realmente roda

A pergunta direta: **existe um servidor Node rodando o tempo todo?**
Não. Em produção, zero processos Node.

| Componente | Linguagem | Roda em produção? | Como |
| --- | --- | --- | --- |
| `cmd/ingest` | Go | **sim, sempre** | 2 tasks no Fargate |
| `cmd/api` | Go | **sim, sempre** | 2 tasks |
| `cmd/worker` | Go | **sim, sempre** | 1 a 10 tasks, autoscale por profundidade da fila |
| `cmd/dlq` | Go | não | CLI operacional, você invoca |
| `apps/dashboard` | TypeScript/Angular | **não roda** | vira 93 kB de arquivos estáticos; quem executa é o navegador |
| `services/simulator` | TypeScript/Node | **não** | ferramenta de desenvolvimento; em produção quem manda webhook é a Verifi ou a Ethoca de verdade |

Os ~300 MB de `node_modules` no disco não vão para lugar nenhum: existem para
compilar o front e para rodar o simulador localmente.

### A Fase 1: o `cmd/ingest`

O problema: um alerta de disputa chega e você tem 24 horas. Se a requisição se
perder, ninguém descobre até o prazo passar.

A ordem dos passos **é** o design de segurança:

```
1. rate limit por IP + limite de 64KB no corpo
2. ler o corpo e o header de assinatura
3. espiar o "type" para escolher o decoder
4. decodificar e validar (valor positivo, moeda de 3 letras, prazo no futuro)
5. buscar o merchant  ──┐
6. verificar o HMAC   ──┴─ os dois devolvem o mesmo 401
7. gastar a cota do merchant
8. reservar a chave de idempotência no Redis
9. gravar tudo em uma transação
```

**Por que 1 vem antes de tudo.** Cada passo seguinte custa uma ida ao banco.
Antes da assinatura ser verificada você não sabe quem é do outro lado, e o
limite por IP é a única proteção que existe nesse momento.

**Por que 2 a 4 vêm antes de 6, mesmo sem confiar em nada.** O `merchant_id` que
escolhe *qual segredo usar* está dentro do corpo — não dá para verificar a
assinatura antes de ler. Mas nada do que foi lido é *usado* até o passo 6; só
serve para escolher a chave.

**Por que 5 e 6 devolvem o mesmo 401.** Se merchant inexistente devolvesse 404 e
assinatura errada devolvesse 401, qualquer um descobriria quais merchants
existem, um chute por vez. O endpoint viraria um oráculo.

**Por que 7 vem depois de 6.** Se a cota do merchant fosse gasta antes da
assinatura, eu poderia mandar requisições dizendo `merchant_id: "mrc_lumen"` e
derrubar a cota deles sem ter chave nenhuma.

### A chave de idempotência vem do corpo assinado

```go
claimed, err := h.guard.Claim(ctx, event.ID)   // event.ID vem de dentro do corpo
```

O header `Idempotency-Key` existe, mas **não é coberto pela assinatura**. Usar
ele permitiria a qualquer um adivinhar um id e fazer o sistema descartar um
evento real como duplicado.

Redis é o caminho rápido; o índice único em `webhook_events.idempotency_key` é a
garantia real. Apague o Redis e o sistema continua correto, só mais lento.

E o detalhe que quase ninguém lembra:

```go
if releaseErr := h.guard.Release(ctx, event.ID); releaseErr != nil { ... }
```

Se você reserva a chave e a escrita falha, sem devolver a reserva o retry do
emissor recebe "já processei" pelas próximas 24 horas. O evento sumiu, e todo
log diz que deu certo.

### O outbox: por que não publicar direto no SQS

Não dá para commitar no Postgres e publicar no SQS atomicamente. As duas
alternativas ingênuas quebram:

- publicar antes do commit: publica, o commit falha, o worker recebe evento de
  uma disputa que não existe
- commitar antes de publicar: commita, o processo morre, a disputa existe e
  ninguém nunca soube

O outbox resolve gravando a mensagem **na mesma transação** que a disputa: ou as
duas existem, ou nenhuma. Um relay separado lê linhas já commitadas e publica.

Foi isso que pagou na Fase 4: quando SQS substituiu o log, **o relay não mudou
uma linha**. O padrão foi construído primeiro, então a fila embaixo dele era um
detalhe trocável.

---

## 8. Angular sem SSR, e o peso comparado

O projeto foi criado com `--ssr=false`. Decisão, não descuido.

**SSR** (Server-Side Rendering) significaria adicionar um **quarto processo
longo, em Node**, que renderiza o HTML no servidor antes de mandar ao navegador.

O que se ganha: primeira pintura mais rápida, funciona sem JavaScript, e SEO.

O que se paga: mais um container para deployar, monitorar, escalar e pagar; mais
um processo Node em produção — exatamente o que hoje não existe; as chamadas de
API acontecem duas vezes, servidor e depois navegador, a menos que se transfira
o estado; e bugs de hidratação, quando o HTML do servidor não bate com o que o
navegador renderiza.

Para este projeto nada disso vale: é painel interno, atrás de login, para duas
pessoas de operações. SEO é irrelevante porque o Google nunca vai ver a página,
e ninguém abre um painel de disputas com JavaScript desligado.

### Os números reais

| | Tamanho | Roda onde | Custo em produção |
| --- | --- | --- | --- |
| `cmd/ingest` (Go) | 13,6 MB | Fargate, 24h por dia | horas de container |
| `cmd/api` (Go) | 14,2 MB | Fargate, 24h por dia | horas de container |
| `cmd/worker` (Go) | 13,1 MB | Fargate, 24h por dia | horas de container |
| **Angular** | **93 kB** gzipped | **navegador do operador** | perto de zero, S3 e CDN |

Não é comparação justa, e é aí que está o ponto: são coisas de natureza
diferente. Os binários Go são o **servidor** — 13 MB no disco, mas gastando CPU e
memória continuamente, cobrados por hora. O Angular são **93 kB baixados uma vez**
por navegador; depois disso o custo é zero, porque quem executa é a máquina do
usuário.

Com SSR ligado, o front deixaria de ser 93 kB estáticos e viraria um quarto
container Node rodando 24 horas, com o mesmo perfil de custo dos três serviços
Go — para renderizar um painel que cinco pessoas abrem por dia.

---

## 9. A Fase 2: a API de leitura e o painel

### Por que dois binários Go e não um

O `cmd/ingest` só escreve e tem segundos para aceitar um webhook. O `cmd/api` só
lê e responde a um painel. Separados, uma consulta pesada de operador nunca
segura uma disputa entrando, e cada um escala por conta própria.

### A consulta que fazia o trabalho antes do LIMIT

A primeira versão de `/api/disputes` era uma consulta única e chata. O `EXPLAIN`
mostrou o problema:

```
Nested Loop  (rows=12991)
  ->  Hash Join  (rows=12991)
  ->  Index Only Scan on transactions  (loops=13119)   <-- 13.119 buscas
Sort  ->  top-N heapsort                                <-- e aí descarta 13.069
```

O Postgres juntava **todas** as 13.119 disputas às suas transações e **depois**
ordenava e jogava fora todas menos cinquenta. Treze mil buscas em índice para
mostrar uma página.

A consulta de contagem tinha o mesmo defeito por outro motivo: fazia join numa
tabela de 500 mil linhas cujas colunas ninguém lia — 39.358 buffers tocados à
toa.

A correção foi em dois estágios: a consulta interna acha só os cinquenta ids, e
só esses cinquenta são unidos ao merchant e à transação para exibição. O join
com `transactions` só aparece quando existe busca textual de verdade.

```
antes:   35 ms
depois:  3 a 7 ms
```

> **A lição:** o trabalho deve vir **depois** do `LIMIT`, não antes.

### OFFSET tem um teto, e a exportação é a resposta

`OFFSET` faz o Postgres percorrer e descartar cada linha antes da janela — a
página 10.000 é uma varredura de tabela usando um número de página como
disfarce. Por isso o offset é limitado a 10.000, e quem realmente quer 40 mil
disputas recebe o CSV, que sai em streaming linha a linha: 13.119 linhas em
90 ms com memória constante.

### `httpResource` e a URL reativa

No Angular, a URL da requisição é **derivada dos signals de filtro**:

```ts
readonly list = httpResource<DisputeList>(
  () => `${this.base}/api/disputes?${this.filters.listQuery()}`,
  { defaultValue: EMPTY_LIST },
);
```

Mudou um filtro, a URL computada muda, a requisição é refeita e **a anterior é
abortada**. Não existe `subscribe`, não existe `unsubscribe`, e — o ponto que
importa — não existe a chance de uma resposta lenta antiga chegar depois de uma
rápida nova e pintar dados velhos por cima dos novos.

RxJS aparece **uma vez só**, para dar debounce na busca:

```ts
toSignal(toObservable(this.searchInput).pipe(debounceTime(250)))
```

Signals não modelam tempo. É a única coisa que RxJS faz melhor aqui, e é
exatamente onde ele foi usado.

### Filtros na URL

Um operador que encontra algo grave manda o link, e quem abre vê as mesmas
linhas. Estado guardado só em campo de componente não pode ser compartilhado,
nem favoritado, nem recarregado. Com `replaceUrl`, filtrar não vira vinte
"voltar" para sair da página.

### Dinheiro continua inteiro até o último instante

O valor viaja como `amount_minor` inteiro mais a moeda, e a **única** divisão
acontece no formatador. O mapa de casas decimais por moeda existe por isso:

```ts
formatMinor(5000, "JPY")  // "¥5,000"  — e não "¥50"
```

Um "divide por 100" genérico erra por um fator de cem e **não parece errado**.

### Dois bugs que valem a história

**O 500 no detalhe da disputa.** Esta consulta:

```sql
WHERE lt.external_ref LIKE 'dispute:' || $1 || ':%'
```

O Postgres infere `$1` como **texto** por causa da concatenação, e o pgx estava
segurando um `int64`. Toda requisição de detalhe era 500. O que torna esse bug
interessante é que eu tinha "verificado" a consulta no psql — mas um literal não
é um parâmetro, e um `PREPARE p(bigint)` *declara* justamente o tipo que a
consulta deixa em aberto. As duas verificações resolviam a ambiguidade que era o
bug. Só rodando pelo pgx aparece.

> **A regra que sobrou:** nunca deixe o tipo de um parâmetro ser decidido pelo
> SQL ao redor dele.

**O CORS que bloqueava o próprio upload.** O middleware anunciava
`Access-Control-Allow-Methods: GET, OPTIONS`, mas o endpoint que assina o upload
é `POST`. O navegador recusaria no preflight — e é isso que torna essa classe de
bug silenciosa: **o servidor nunca vê a requisição bloqueada**, então não há nada
no log. Parece que o botão não faz nada.

---

## 10. A Fase 3: o worker que decide antes do prazo

O processo que faz disso uma plataforma em vez de um arquivo morto.

### A fila é um sorted set do Redis

`ZADD disputes:deadlines <timestamp> <id>` — o score é o prazo. Perguntar "o que
venceu?" é `ZRANGEBYSCORE`, em tempo logarítmico.

Mas pegar o trabalho é um **script Lua**:

```lua
local ids = redis.call('ZRANGEBYSCORE', key, '-inf', upto, 'LIMIT', 0, limit)
if #ids > 0 then redis.call('ZREM', key, unpack(ids)) end
return ids
```

Feito como duas chamadas do Go — ler e depois remover — existe uma janela entre
elas, e dois workers fazendo polling ao mesmo tempo leem os mesmos ids e fazem o
mesmo trabalho duas vezes. Dentro de um script é uma operação atômica: um id vai
para exatamente um worker. Existe um teste com 8 goroutines e 500 ids que prova
isso.

### O índice é descartável, e isso é de propósito

O sorted set não é registro, é **índice**. Um passo de reconciliação reconstrói
ele inteiro a partir das disputas abertas no Postgres:

```sql
SELECT id, deadline_at FROM disputes WHERE state IN ('received','resolving')
```

Apague o Redis e custa uma passada, nada mais. Isso também fecha a lacuna que o
caminho rápido não cobre: uma disputa gravada enquanto o Redis estava fora nunca
foi agendada, e sem essa passada ficaria esquecida até o prazo passar.

> Se perder aquele dado dói, ele não deveria estar só ali.

### O lock é a camada **mais fraca**, não a mais forte

Essa é a parte que a maioria erra numa entrevista.

Qualquer lock com timeout pode ser segurado por dois processos ao mesmo tempo: o
dono pausa numa coleta de lixo, o TTL expira, e um segundo worker adquire o lock
**legitimamente**. Não é bug, é a natureza da coisa.

O que **de fato** impede um reembolso duplicado são duas camadas abaixo:

1. A checagem otimista de versão: `WHERE id = $1 AND version = $2`
2. O `external_ref` único no ledger: `dispute:1234:refund` só pode existir uma vez

O lock só significa que o segundo worker normalmente nem tenta. Ele ainda libera
com segurança — comparando o token antes de apagar, porque um `DEL` cru apaga
qualquer lock que estiver ali, inclusive o que outro worker acabou de pegar.

### A política é uma função pura

`rules.go` não tem banco nem Redis dentro. Recebe uma struct e devolve uma
decisão. O que o sistema faz com o dinheiro de alguém é a parte que mais precisa
ser legível, revisável e testável sem subir infraestrutura.

| Situação | Decisão |
| --- | --- |
| Prazo vencido, ainda aberta | **expirar** — um fracasso registrado, não um resultado escolhido |
| Alerta, dentro do teto do merchant, com saldo na cobrança | **reembolsar** |
| Alerta, acima do teto ou sem saldo restante | **escalar** |
| Chargeback, código de motivo baseado em evidência | **contestar** |
| Chargeback, código de fraude | **escalar** |

**Ele nunca desiste de um chargeback.** Reembolsar um alerta dentro da janela é
estritamente mais barato do que deixar vencer, então é seguro automatizar.
Desistir de um chargeback é um julgamento sobre evidência e sobre a relação com
o merchant — e um motor de regras que silenciosamente dá dinheiro por perdido é
justamente o que ninguém percebe estar errado.

Resultado sobre 1.667 disputas abertas: **395 reembolsos, 412 contestações, 860
escalações, zero erros** — e as onze invariantes de dinheiro continuaram passando
depois.

### O livelock: a melhor história do projeto

A primeira versão **não fazia nada, não logava nada, e consumia um núcleo
inteiro**.

`SIGQUIT` despejou as pilhas de todas as goroutines: todas paradas em `SETNX`.
Parecia Redis travado — mas o Redis respondia `PING` instantaneamente e tinha
quatro clientes. As pilhas eram uma pista falsa.

Quem respondeu foi o `MONITOR`:

```
zadd disputes:deadlines 1788667662 9467
evalsha ... lock:dispute:9467
zadd disputes:deadlines 1788667662 9467
evalsha ... lock:dispute:9467
```

Os **mesmos quatro ids** sendo pegos, reagendados e pegos de novo, alguns
milhares de vezes por segundo.

A causa: pegar trabalho leva tudo que vence antes de `agora + lookahead`. A
escalação reagendava para o **prazo da disputa**, que está *dentro* dessa
janela — então voltava a ser elegível imediatamente. Pior: como todo lote voltava
cheio, o laço interno nunca terminava, o laço de polling nunca voltava ao seu
`select`, e as outras 1.663 disputas **nunca foram olhadas**.

Duas correções, ambas invariantes e não remendos:

1. Todo reagendamento passa por uma função que garante um horário **fora** da
   janela de captura
2. Um tick de polling drena um número limitado de lotes, então o laço sempre
   volta ao `select`

```
antes:  0 decisões
depois: 807 decisões, 1.667 capturadas, 0 erros
```

E o heartbeat existe por causa disso: **um worker que só loga quando age é
indistinguível de um worker travado.**

---

## 11. As frases para levar

**Terraform**

1. *"Terraform não é um serviço, é uma ferramenta de linha de comando — como
   `docker compose`, mas para a nuvem em vez do meu notebook. Roda, chama a API
   da AWS, e morre."*
2. *"O valor é o `plan`: eu vejo exatamente o que vai mudar antes de mudar. É
   isso que permite revisar infraestrutura num pull request. O resto é
   consequência."*
3. *"O custo é o state file — guarda segredo em texto puro e é uma
   responsabilidade real. Vale citar em vez de fingir que não existe."*

3b. *"Terraform provisiona, não popula: cria a fila vazia e o bucket vazio. Se
    `destroy` seguido de `apply` recria aquilo, é infraestrutura; se não recria,
    é dado."*

**Quando usar (e quando não)**

3c. *"Três perguntas: mais de um ambiente, mais de uma pessoa, e o custo de
    recriar na mão. Se as três forem não, é overhead — um blog na Vercel não
    precisa. O que muda o jogo não é o tamanho do app, é o segundo ambiente e a
    segunda pessoa. E dá para adotar depois com `import`, então começar sem não
    é dívida técnica, é sequenciamento."*

3d. *"Neste projeto: 8 recursos rodam o código e 52 são permissão, tráfego e
    observabilidade. Rodar o código é a parte fácil."*

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

**Arquitetura**

7. *"Em produção não roda nenhum processo Node. TypeScript existe em dois
   lugares: o front, que compila para 93 kB estáticos e roda no navegador, e o
   simulador, que é ferramenta de desenvolvimento e some quando o processador
   real assume."*

8. *"A ordem dos passos no ingest é o design de segurança. Merchant inexistente
   e assinatura errada devolvem o mesmo 401, senão o endpoint vira um oráculo
   para descobrir quais merchants existem."*

9. *"A chave de idempotência vem do corpo assinado, nunca do header — o header
   não é coberto pela assinatura, então quem adivinhasse um id poderia fazer o
   sistema descartar um evento real como duplicado."*

10. *"Outbox porque não dá para commitar no Postgres e publicar no SQS
    atomicamente. Publicar antes do commit cria evento de disputa que não
    existe; commitar antes de publicar perde a disputa em silêncio. E foi isso
    que fez a troca para SQS não mudar uma linha do relay."*

11. *"Sem SSR porque é painel interno atrás de autenticação. O ganho de primeira
    pintura não paga um quarto processo Node em produção — sem SSR o front é
    93 kB num CDN e custa praticamente nada."*

**Banco e performance**

12. *"O trabalho tem que vir depois do LIMIT. O EXPLAIN mostrou o Postgres
    juntando 13 mil disputas às transações e só então ordenando e descartando
    todas menos cinquenta. Em dois estágios: 35ms para 3ms."*

13. *"Nunca deixe o tipo de um parâmetro ser decidido pelo SQL ao redor. Um LIKE
    com concatenação faz o Postgres inferir texto, e o driver estava mandando
    int64 — toda requisição de detalhe era 500. Nem literal no psql nem PREPARE
    com tipo declarado reproduzem, porque os dois resolvem a ambiguidade que é o
    bug."*

**Concorrência**

14. *"Pegar trabalho da fila é um script Lua porque ler e remover em duas
    chamadas deixa uma janela onde dois workers leem os mesmos ids."*

15. *"O lock distribuído é a camada mais fraca, não a mais forte. Qualquer lock
    com timeout pode ser segurado por dois processos — o dono pausa, o TTL
    expira, outro adquire legitimamente. O que impede reembolso duplicado é a
    checagem de versão e o external_ref único no ledger."*

16. *"Tive um livelock que não logava nada e queimava um núcleo. As pilhas do
    SIGQUIT eram pista falsa; quem respondeu foi o MONITOR do Redis, mostrando
    os mesmos quatro ids sendo reagendados milhares de vezes por segundo. O
    reagendamento caía dentro da própria janela de captura."*

---

## 12. Go idiomático: onde este código fugia dele

Uma revisão de Go idiomático passou pelo módulo inteiro em 2026-09-13. Ela não
achou nenhum problema de build, de `gofmt` ou de `go vet` — os 120 achados eram
todos sobre **forma**: onde os nomes moram, o que o valor zero de um struct
significa, quais erros atravessam a fronteira de um pacote, e se o compilador
estava sendo informado o bastante para ajudar. A versão completa, em inglês e
com o arquivo de cada exemplo, está em
[`go-conventions.md`](go-conventions.md). Aqui fica o resumo.

### Visibilidade é por **pacote**, não por arquivo

Essa é a que mais precisa ser desaprendida vindo de Node. Lá, um `const` que o
módulo não exporta é invisível para qualquer outro arquivo — a unidade é o
arquivo. Em Go a unidade é o **diretório**. Todo `.go` dentro de
`internal/worker` é o mesmo pacote, e cada um enxerga os nomes minúsculos dos
outros sem import e sem export: `worker.go` lê o struct `loaded` declarado em
`store.go` porque são o mesmo pacote, não porque alguém compartilhou algo.

Então minúscula não quer dizer "privado de mim". Quer dizer "privado deste
pacote" — e o pacote passa a ser a unidade que se revisa e se mantém honesta. O
arquivo é só uma decisão de arquivamento.

`internal/` é uma segunda parede por cima disso, imposta pelo compilador e
baseada no **caminho**: nada fora do módulo importa nada sob `internal/`. E a
regra se aplica em qualquer nível, que é a parte útil: `cmd/internal/boot` só
pode ser importado por código sob `cmd/`, e foi exatamente por isso que os dez
binários puderam dividir um bootstrap comum sem que ele virasse API do módulo.

Consequência: dentro de `internal/`, um nome com inicial maiúscula é uma
**promessa** aos outros pacotes deste módulo. A revisão achou cerca de setenta
nomes maiúsculos sem nenhum leitor fora do próprio pacote. Continuam exportados
de propósito: constantes de enum de um tipo exportado e os sentinelas `Err` —
um sentinela existe justamente para ser comparado de fora com `errors.Is`.

**O formato específico do bug:** função exportada que devolve tipo não
exportado. `worker.Store.Load` devolvia `loaded`, e `Store.Apply` recebia um.
Os métodos eram públicos, o tipo não. Go compila isso sem reclamar, mas quem
estivesse em outro pacote não conseguiria declarar uma variável daquele tipo —
só poderia devolver o valor de volta sem olhar. A correção é escolher qual lado
está errado: aqui ninguém de fora chamava, então os dois desceram para `load` e
`apply`. No caso do `agent.Trace` foi o contrário — o tipo merecia subir, e
`cmd/agent/report.go` deixou de manter uma cópia privada do struct sincronizada
na mão.

Por isso também os **dublês de teste** saíram do pacote de produção para um
subpacote (`internal/agent/agenttest`): um fake exportado é superfície pública
que vai junto no binário. Detalhe de Go: arquivos de teste vêm em dois sabores,
`package agent` (enxerga os nomes minúsculos) e `package agent_test` (só a
API). Os dezessete arquivos de teste daquele pacote são do primeiro tipo, e o
primeiro tipo não pode importar `agenttest`, porque `agenttest` importa `agent`
— ciclo.

### O valor zero tem que funcionar

Go não tem construtor obrigatório nem campo obrigatório. Qualquer pacote que
consiga nomear o tipo escreve `T{}`. O valor zero é parte da API, tendo sido
projetado ou não.

`api.Filters{}` renderizava `ORDER BY  DESC` e `LIMIT 0`, porque os defaults
moravam no parser de query string e não no tipo — e `internal/disputetools`
montava o struct direto. E `worker.Consumer{}` era pior porque *funcionava*:
`WaitTime` zero é long poll de zero segundo, ou seja, um busy loop contra o
SQS. Duas correções diferentes de propósito: o filtro normaliza no uso (montar
um literal com três campos é razoável), o consumidor ganhou `NewConsumer` com
as opções não exportadas (não existe literal razoável — todo campo é uma
decisão sobre um serviço remoto).

### Vocabulário escrito à mão sempre diverge (a melhor história)

A migration `000003` adicionou o estado `draft_ready` em 2026-09-08, e o valor
não chegou a **nenhum** dos cinco lugares que listavam estados em prosa: o
whitelist de filtros da API (então `?state=draft_ready`, que é a fila de
revisão, respondia 400), a union `DisputeState` do dashboard e seu controle de
filtro, a tag `jsonschema` da ferramenta `list_disputes` (que dizia ao modelo
que o estado não existia), e o invariante de dinheiro do simulador.

O último é o que importou. A checagem "money held equals the chargebacks still
open" listava `received`, `resolving` e `represented`, então treze chargebacks
parados na fila de revisão seguravam 1.184,38 USD que a checagem contava como
*não* segurados — e as quatro divergências por merchant somavam exatamente esse
número. O `make verify` estava falhando havia cinco dias. **O ledger estava
certo o tempo todo; a checagem é que estava velha.** Com `draft_ready` incluído,
os onze invariantes passam.

A correção é o pacote `internal/dispute`, com `type State string` e as
constantes que substituem uns sessenta literais espalhados por Go e pelo SQL
dentro do Go. Os valores foram conferidos contra os `CHECK` das migrations, que
são a autoridade.

O que vale gravar é o limite da técnica. Um tipo string nomeado transforma o
compilador num leitor a mais: erro de digitação vira erro de compilação no
lugar onde foi digitado, e não um resultado vazio uma hora depois. Mas ele
**não alcança** tag de struct, union de TypeScript nem string de SQL. Dos cinco
leitores que perderam o `draft_ready`, dois eram TypeScript e um era uma tag —
uma string que o compilador nunca lê. Então a afirmação honesta não é "constante
tipada teria evitado o bug": é que ela teria pego o whitelist, e que quem
percebeu o dinheiro foi o invariante **falhando**. O valor daquela checagem é
ter falhado; uma suíte verde não diria nada.

A mesma intuição vale para parâmetros. Toda função de lançamento do ledger
recebia `disputeID`, `merchantID` e `amountMinor` como três `int64` vizinhos:
trocar dois compila e lança dinheiro no merchant errado, em silêncio, num razão
de partidas dobradas cuja razão de existir é não poder errar em silêncio. Hoje
recebem um `Entry`, e sobrou uma regra curta: **um parâmetro sem vizinho do
mesmo tipo não tem com quem ser trocado.**

### Byte não é rune, e um slice pode ser a memória de outro

String em Go é um slice de bytes: `range` entrega runes, mas `s[i]` entrega um
byte e `s[:n]` corta num offset de byte — e é essa inconsistência que pega.
Mascarar e-mail com `local[:1]` cortou o `é` de `é@example.com` no meio e
produziu UTF-8 inválido dentro de um resultado de ferramenta JSON.

A outra metade é menos visível: um `[]T` recebido é uma janela para o array de
outra pessoa, e `append` pode escrever nele em vez de alocar. Em
`internal/api/decisions.go` o limit e o offset eram anexados direto no mesmo
array que os argumentos da query de contagem ainda apontavam — seguro só porque
aquela query já tinha retornado, o que é um fato sobre ordem de execução e não
sobre o código. Dois arquivos adiante, `ListDisputes` roda as duas em paralelo.
Regra: se você guarda ou faz crescer um slice que recebeu, copie antes.

### Erro carrega significado

`%w` embrulha em vez de achatar, `errors.Is` percorre a cadeia (por isso `==`
está errado mesmo quando funciona: só compara o erro de fora) e `errors.As` faz
o mesmo passeio procurando um *tipo* e devolve o valor tipado.

A regra que mais rende: traduzir o sentinela do driver na fronteira do store.
`Store.Load` devolvia `pgx.ErrNoRows` para fora do pacote, então o worker
importava o driver só para comparar — todo consumidor do store era obrigado a
saber qual biblioteca de banco estava embaixo. Hoje é `worker.ErrNotFound`,
embrulhado com o id da disputa, e o import do pgx sumiu.

E há o erro que nunca chega. Em `internal/config/config.go`, um valor
**presente mas ilegível** devolvia o default: `WORKER_LOCK_TTL=30` (sem
unidade) virava silenciosamente os 30 s padrão. É assim que uma configuração
errada se esconde — não como valor errado, mas como default plausível. Hoje as
falhas de parse são juntadas com `errors.Join` e voltam de `Load`, então um
restart reporta todas de uma vez. Ausente ou vazio continua significando
default; presente e errado é erro.

### Context não é só um parâmetro

Context é um sinal de cancelamento que por acaso viaja como argumento. Passar
adiante é a parte fácil; difícil é notar os dois lugares onde o cancelamento
está errado.

O primeiro é limpeza. O release do lock e os reagendamentos do worker rodavam
sob o contexto da requisição. No shutdown esse contexto já está cancelado —
então todo handler em voo falhava em soltar o lock, e cada disputa ficava
travada pelo TTL inteiro, no exato momento em que o lock é menos útil e mais
atrapalha. Hoje rodam sob `context.WithoutCancel` com dois segundos de timeout:
`WithoutCancel` mantém os valores e descarta o cancelamento, e o timeout existe
porque o contexto de origem não carrega mais prazo nenhum.

O segundo é o oposto — terminar cedo e não avisar. O backfill saía do loop
quando o contexto acabava e devolvia `stats, nil`, então `cmd/embed` imprimia
resumo de sucesso para uma execução interrompida. Devolve `stats, ctx.Err()`
agora: `ctx.Err()` é como uma função diz "parei porque mandaram parar", e `nil`
diz "terminei".

### Comentário que mente

Apareceram três formatos, e eles não são igualmente graves. Comentário
descrevendo um componente que não existe (frases prometendo Bedrock em produção,
sendo que o provider foi construído e apagado) e comentário citando um passo de
plano que foi removido apenas gastam o tempo de quem lê.

O terceiro faz estrago: `internal/api/reviews.go` afirma que `ReviewFinding` e
`agent.Finding` "são fixados juntos por um teste", e esse teste não existe em
lugar nenhum do módulo. Os dois structs são redeclarados em vez de
compartilhados porque `agent` importa `api` e o contrário seria ciclo — ou
seja, *só* um teste pode pegar um campo divergindo, e o comentário é exatamente
o motivo de ninguém ter ido procurar. Comentário que afirma uma garantia é
carga estrutural; falso, é pior que silêncio, porque silêncio pelo menos
convida à conferência.

E há uma regra de posição que é específica de Go: um comentário vira
documentação **pela posição** — colado na declaração e começando pelo nome
declarado. Parágrafo solto no fim do arquivo é invisível para `go doc`, isto é,
invisível justamente para quem mais o queria. Ler `go doc ./internal/<pkg>` e
perguntar se aquilo funciona como índice é a revisão mais barata deste
repositório: pega o comentário faltando, o nome exportado que não devia estar, e
a função pública devolvendo tipo que o leitor não consegue nomear.

---

## 13. Onde está o resto do contexto

Este documento explica *conceitos* — o porquê de cada tecnologia, em português,
para reler antes de uma entrevista. Ele não é o registro das decisões do projeto.

- **[`DECISIONS.md`](DECISIONS.md)** — o log de decisões: cada escolha real feita
  ao longo das 5 fases, o que foi rejeitado e por quê, e os bugs cuja lição virou
  regra (o tipo de parâmetro em SQL, o livelock do worker, a fronteira read-only
  do MCP). Em inglês, porque referencia identificadores do código.
- **[`go-conventions.md`](go-conventions.md)** — a versão longa da seção 12: o
  que a revisão de Go idiomático ensinou, com o arquivo de cada exemplo, de
  visibilidade por pacote a comentário que mente. Em inglês, como o
  `DECISIONS.md`, e pelo mesmo motivo.
- **[`../.spec/agent-harness/part-b.md`](../.spec/agent-harness/part-b.md)** — o
  desenho completo da Parte B (o harness agêntico): o loop manual, o verificador
  independente, as guardrails, os evals e a ordem de construção. Desenhado, não
  construído. Tem uma seção final em português com as frases para a entrevista.
- **[`../README.md`](../README.md)** — o que existe hoje e como rodar.

A regra de separação: `docs/` descreve **o que é**; `.spec/` descreve **o que
ainda não é**. Quando algo do `.spec/` for construído, a decisão que valeu a pena
guardar migra para `docs/` e o plano é apagado — o git é o arquivo.
