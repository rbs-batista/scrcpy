# Scrcpy Fake GPS

Interface desktop para controlar dispositivos Android via ADB, com streaming de tela em tempo real e simulação de localização GPS via mapa interativo.

## Funcionalidades

- **Streaming de tela** — transmite a tela do dispositivo Android via H.264 (scrcpy-server) com WebCodecs no browser
- **Fake GPS** — clique no mapa para definir a localização do dispositivo instantaneamente
- **Modo Rota** — defina múltiplos pontos no mapa e simule movimento ao longo do trajeto
- **Controle por toque** — tap e swipe encaminhados ao dispositivo em tempo real via protocolo binário scrcpy
- **Botões de navegação** — Voltar, Home e Recentes
- **Múltiplos dispositivos** — lista e alterna entre todos os dispositivos ADB conectados
- **Busca de localização** — pesquisa de cidades e endereços via OpenStreetMap (Nominatim)

## Arquitetura

```
map_interface/
├── main.go              # Servidor HTTP + lógica de streaming e ADB
├── index.html           # Interface web (Leaflet map + controles)
├── sysutil_windows.go   # HideWindow para subprocessos no Windows
├── sysutil_other.go     # no-op para outros sistemas
├── versioninfo.json     # Metadados do exe + caminho do ícone
├── gen_icon.go          # Gerador do icon.ico (go run gen_icon.go)
├── icon.ico             # Ícone gerado (smartphone azul, 7 tamanhos)
├── build.bat            # Script de build Windows (sem janela de console)
├── go.mod / go.sum      # Dependências Go
└── scrcpy/
    └── scrcpy-server    # JAR do servidor scrcpy (enviado ao device via adb push)
```

O app abre uma janela nativa via `webview_go` apontando para um servidor HTTP local (`localhost:8080`). O streaming usa o protocolo scrcpy com `raw_stream=true` (H.264 Annex B direto) decodificado via WebCodecs no browser. A localização fake é enviada via UDP para `127.0.0.1:5554`.

## Pré-requisitos

- Go 1.21+
- Windows 10/11
- ADB — bundled na pasta `scrcpy/` ou instalado no sistema (`ANDROID_SDK_ROOT`)

## Build

### Windows — executável sem janela de console (recomendado)

```bat
cd map_interface
build.bat
```

Gera `DM Tools.exe` com ícone embutido, sem janela de terminal. O `build.bat`:
- Gera `icon.ico` automaticamente via `gen_icon.go` (se não existir)
- Instala `goversioninfo` automaticamente se necessário
- Embute ícone e metadados no executável via `resource.syso`

### Windows — build de desenvolvimento (com console para logs)

```bat
cd map_interface
go build -o "DM Tools.exe" .
```

### Executar direto sem compilar (desenvolvimento)

```bat
cd map_interface
go run .
```

## Executar

```bat
"map_interface\DM Tools.exe"
```

Ao fechar a janela, todos os processos scrcpy são encerrados automaticamente.

## Endpoints HTTP

| Método | Rota | Descrição |
|--------|------|-----------|
| GET | `/devices` | Lista dispositivos ADB conectados |
| POST | `/devices/select` | Seleciona o dispositivo ativo |
| POST | `/scrcpy/start` | Inicia streaming do dispositivo |
| POST | `/scrcpy/stop` | Para o streaming |
| GET | `/scrcpy/status` | Status atual do streaming |
| GET | `/ws/video` | WebSocket H.264 stream da tela |
| POST | `/location` | Envia coordenadas GPS (lat, lng) |
| POST | `/touch` | Envia tap/swipe/keyevent ao dispositivo |
| GET | `/device/size` | Resolução do dispositivo |
| GET | `/window/phone-size` | Tamanho da janela nativa |
| POST | `/window/resize` | Redimensiona a janela nativa |
