# Scrcpy Fake GPS

Interface desktop para controlar dispositivos Android via ADB, com streaming de tela em tempo real e simulação de localização GPS via mapa interativo.

## Funcionalidades

- **Streaming de tela** — transmite a tela do dispositivo Android via H.264 (scrcpy-server) com fallback para screencap via ADB
- **Fake GPS** — clique no mapa para definir a localização do dispositivo instantaneamente
- **Modo Rota** — defina múltiplos pontos no mapa e simule movimento ao longo do trajeto
- **Controle por toque** — tap e swipe encaminhados ao dispositivo em tempo real
- **Botões de navegação** — Voltar, Home e Recentes
- **Múltiplos dispositivos** — lista e alterna entre todos os dispositivos ADB conectados
- **Busca de localização** — pesquisa de cidades e endereços via OpenStreetMap (Nominatim)

## Arquitetura

```
map_interface/
├── main.go          # Servidor HTTP + lógica de streaming e ADB
├── index.html       # Interface web (Leaflet map + controles)
├── go.mod / go.sum  # Dependências Go
├── adb              # Binário ADB bundled
└── Scrcpy Fake GPS.app/   # App bundle macOS
    └── Contents/MacOS/
        ├── map-server   # Binário compilado
        ├── index.html   # Frontend copiado
        └── adb          # ADB bundled
```

O app abre uma janela nativa via `webview_go` apontando para um servidor HTTP local (`localhost:8080`). O streaming usa o protocolo do scrcpy-server (H.264 raw → ffmpeg → MJPEG) com fallback para `adb exec-out screencap -p`.

A localização fake é enviada via UDP para `127.0.0.1:5554`, protocolo compatível com o emulador Android.

## Pré-requisitos

- Go 1.21+
- macOS
- `ffmpeg` instalado (`brew install ffmpeg`)
- ADB — pode usar o bundled (`./adb`) ou o do sistema

## Build

### Compilar o binário

```bash
cd map_interface
go build -buildvcs=false -o map-server .
```

### Copiar para o app bundle

```bash
cp map-server "Scrcpy Fake GPS.app/Contents/MacOS/map-server"
cp index.html "Scrcpy Fake GPS.app/Contents/MacOS/index.html"
```

### Build completo em um comando

```bash
cd map_interface && \
go build -buildvcs=false -o map-server . && \
cp map-server "Scrcpy Fake GPS.app/Contents/MacOS/map-server" && \
cp index.html "Scrcpy Fake GPS.app/Contents/MacOS/index.html" && \
codesign --force --deep --sign - "Scrcpy Fake GPS.app"
```

## Executar

```bash
# Direto pelo binário
cd map_interface
./map-server

# Ou abrindo o app bundle
open "map_interface/Scrcpy Fake GPS.app"
```

## Endpoints HTTP

| Método | Rota | Descrição |
|--------|------|-----------|
| GET | `/devices` | Lista dispositivos ADB conectados |
| POST | `/devices/select` | Seleciona o dispositivo ativo |
| POST | `/scrcpy/start` | Inicia streaming do dispositivo |
| POST | `/scrcpy/stop` | Para o streaming |
| GET | `/scrcpy/status` | Status atual do streaming |
| GET | `/stream` | MJPEG stream da tela |
| POST | `/location` | Envia coordenadas GPS (lat, lng) |
| POST | `/touch` | Envia tap/swipe/keyevent ao dispositivo |
| GET | `/device/size` | Resolução do dispositivo |
| GET | `/window/phone-size` | Tamanho da janela nativa |
| POST | `/window/resize` | Redimensiona a janela nativa |
