# wacompress

Compacta os vídeos do WhatsApp de um celular Android conectado por cabo. O programa reconverte cada vídeo para H.265 (HEVC) no PC e coloca o arquivo menor no lugar do original, que fica guardado em uma pasta de backup.

Roda no Linux, é escrito em Go e usa o `adb` e o `ffmpeg` como programas externos.

## Requisitos

- Go 1.26 ou mais novo, para compilar.
- `adb` e `ffmpeg` (com `ffprobe`): `sudo apt install adb ffmpeg`
- Depuração USB ativada no celular. Ao conectar o cabo, aceite o aviso de autorização na tela.
- Para a opção `-gpu`: uma GPU com VAAPI em `/dev/dri/renderD128` (Intel ou AMD).

## Compilar

```
cd ~/go/src/wacompress
go build
```

Isso gera o executável `./wacompress`. Os testes rodam com `go test ./...`.

## Uso

```
./wacompress -dry-run     # só lista o que seria feito, sem alterar nada
./wacompress -limit 1     # converte um vídeo, para conferir o resultado no celular
./wacompress -gpu         # converte tudo pela GPU
```

Na primeira vez, rode com `-limit 1` e confira se o vídeo abre no WhatsApp e aparece na data certa na galeria antes de converter tudo.

Pode interromper com Ctrl+C a qualquer momento. Rodar de novo continua de onde parou.

### Opções

| Opção | Padrão | Função |
|---|---|---|
| `-gpu` | desligado | Converte pela GPU (VAAPI). Muito mais rápido que a CPU. |
| `-qp` | `33` | Qualidade na GPU. Menor = melhor qualidade e arquivo maior. |
| `-crf` | `26` | Qualidade na CPU (`libx265`). Menor = melhor qualidade e arquivo maior. |
| `-preset` | `medium` | Velocidade do `libx265`, de `ultrafast` a `veryslow`. |
| `-limit` | `0` | Processa só N vídeos. `0` processa todos. |
| `-min-size` | `0` | Ignora vídeos menores que o tamanho dado (ex.: `500K`, `1MB`). |
| `-dry-run` | desligado | Só lista o que seria feito. |
| `-serial` | vazio | Escolhe o aparelho, se houver mais de um conectado. |
| `-backup` | a configurada | Pasta de backup só para esta execução. |
| `-set-backup` | | Grava a pasta de backup a usar daqui em diante e sai. |
| `-folders` | | Lista outras pastas do celular com vídeo e pergunta quais adicionar. |
| `-evaluate` | | Compara a qualidade da CPU e da GPU com o original. Não mexe no celular. |
| `-samples` | `5` | Quantos vídeos do backup usar em `-evaluate`. |
| `-freed` | | Mostra o espaço já liberado em cada aparelho. |

## O que é convertido

Vídeos `.mp4`, `.mov` e `.mkv` destas pastas do WhatsApp e do WhatsApp Business, com as subpastas:

- `WhatsApp Video` (inclui `Sent` e `Private`)
- `WhatsApp Documents`
- `WhatsApp Video Notes`
- `WhatsApp Animated Gifs`

A pasta `.Statuses` fica de fora, porque é um cache temporário do WhatsApp.

Para incluir outras pastas do celular (câmera, gravações de tela etc.), rode `./wacompress -folders`. O programa lista as pastas com vídeo que ainda não são analisadas e pergunta quais adicionar. A escolha fica gravada em `pastas.json` na pasta do aparelho; para tirar uma pasta, apague a linha dela nesse arquivo. Dados internos de aplicativos (`/sdcard/Android`), lixeiras e pastas ocultas não são oferecidos.

## Como funciona

Para cada vídeo, do maior para o menor:

1. Baixa o arquivo do celular e guarda o original no backup.
2. Converte para H.265. O áudio é copiado sem alteração.
3. Confere o resultado (veja abaixo).
4. Envia o arquivo novo para um nome temporário no celular e troca pelo original por renomeação.
5. Anota o resultado no registro.

O original no celular só é substituído se o convertido passar em todas estas conferências:

- o codec é H.265 e a duração bate com a do original;
- a faixa de áudio continua lá;
- os metadados foram mantidos, incluindo a data de criação (`creation_time`);
- a profundidade de cor e o HDR foram mantidos;
- o arquivo ficou pelo menos 10% menor;
- o tamanho e a data de modificação conferem no celular depois do envio.

Se alguma falhar, o vídeo aparece na tela como `skipped`, com o motivo, e o original fica intacto. O nome e a extensão do arquivo nunca mudam.

### Paralelismo

Vídeos pequenos são convertidos vários ao mesmo tempo:

| Tamanho | Ao mesmo tempo |
|---|---|
| 15 MB ou mais | 1 |
| abaixo de 15 MB | 2 |
| abaixo de 5 MB | 4 |

Os limites são as constantes `mediumBelow` e `smallBelow` em `main.go`.

### CPU ou GPU

A CPU (`libx265`) gera arquivos menores para a mesma qualidade, mas é bem mais lenta. A GPU (`-gpu`) é cerca de 20 vezes mais rápida. Para medir a diferença nos seus próprios vídeos:

```
./wacompress -evaluate                 # usa 5 originais do backup
./wacompress -evaluate video1.mp4      # ou arquivos indicados
```

A saída mostra, para cada conversão, o tamanho, o tempo e duas medidas de semelhança com o original: SSIM (de 0 a 1; 1 = idêntico) e PSNR (em dB; quanto maior, melhor).

Vídeos com menos de 128 pixels de largura ou altura não são aceitos pela GPU e vão automaticamente pela CPU.

### HDR

Vídeos de 10 bits continuam em 10 bits, e a curva HDR (HDR10 ou HLG) e os metadados HDR10 são mantidos. Limites:

- **HDR10+:** a conversão não consegue manter os metadados dinâmicos. Esses vídeos são pulados.
- **Dolby Vision:** a camada Dolby Vision se perde e fica o HDR comum que vem junto. Se o arquivo não tiver essa base compatível, é pulado.

## Backup e registro

A pasta de backup é configurável. Sem configuração, é `~/wacompress-backup`. Para usar outra, por exemplo em um disco externo:

```
./wacompress -set-backup "/media/usuario/DiscoExterno/wacompress-backup"
```

A escolha fica gravada em `~/.config/wacompress/config.json` e vale para as execuções seguintes. A opção `-backup <pasta>` troca a pasta só naquela execução.

Se a pasta configurada não existir (disco desconectado, por exemplo), o programa para com um aviso em vez de criar um backup vazio em outro lugar. Sem o registro ele não saberia o que já foi convertido e converteria tudo de novo.

Para mudar um backup existente de lugar, copie a pasta inteira para o novo local e rode `-set-backup` apontando para ele.

Cada aparelho tem a sua pasta dentro do backup, com o nome formado pelo modelo e pelo número de série:

```
<pasta de backup>/
  Pixel_8-ABC123XYZ/
    originais/       # os vídeos originais, na mesma estrutura de pastas do celular
    registro.json    # o que já foi processado
    pastas.json      # pastas extras escolhidas com -folders
```

As mensagens do programa são em inglês; os nomes dentro do backup e os campos do `registro.json` continuam em português.

O `registro.json` guarda um de três estados para cada vídeo:

| Estado | Significado | Na próxima execução |
|---|---|---|
| `convertido` (na tela: converted) | Trocado no celular. | Não é refeito. |
| `pulado` (na tela: skipped) | Não valeu a pena ou não passou em uma conferência. | Não é refeito. |
| `falhou` (na tela: FAILED) | Erro durante o processo. | É tentado de novo. |

Para forçar uma nova tentativa de um vídeo `pulado`, apague a entrada dele no `registro.json`.

O backup guarda todos os originais e pode ocupar muito espaço no PC. Apague a pasta `originais` quando estiver satisfeito com o resultado; sem ela não há como voltar atrás.

## Cuidados

- **WhatsApp:** manter o nome do arquivo costuma bastar para o vídeo continuar abrindo na conversa, mas isso não é garantido para toda versão do aplicativo. Teste com `-limit 1` antes.
- **GIFs animados:** o WhatsApp os guarda como `.mp4` e eles também são convertidos. Confira alguns no aplicativo depois da primeira execução.
- **Uma execução por vez:** não rode duas conversões ao mesmo tempo no mesmo aparelho.
- **Celular desconectado:** se o cabo cair no meio, o programa para e avisa. Reconecte e rode de novo.
- **Data de criação:** a data de criação do sistema de arquivos não pode ser definida no Android. O que fica preservado é a data de modificação e o `creation_time` interno do vídeo, que são os que a galeria usa.
