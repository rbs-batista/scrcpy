import http.server
import socketserver
import json
import socket
import urllib.parse
import os

PORT = 8080
UDP_IP = "127.0.0.1"
UDP_PORT = 5554

class MapHandler(http.server.SimpleHTTPRequestHandler):
    def do_POST(self):
        if self.path == '/location':
            content_length = int(self.headers['Content-Length'])
            post_data = self.rfile.read(content_length)
            data = json.loads(post_data.decode('utf-8'))
            
            lat = data.get('lat')
            lng = data.get('lng')
            
            if lat is not None and lng is not None:
                message = f"{lat},{lng}".encode('utf-8')
                sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
                sock.sendto(message, (UDP_IP, UDP_PORT))
                
                self.send_response(200)
                self.send_header('Content-type', 'application/json')
                self.end_headers()
                self.wfile.write(json.dumps({'status': 'ok'}).encode('utf-8'))
            else:
                self.send_response(400)
                self.end_headers()
        else:
            super().do_POST()

if __name__ == "__main__":
    web_dir = os.path.dirname(os.path.abspath(__file__))
    os.chdir(web_dir)
    with socketserver.TCPServer(("", PORT), MapHandler) as httpd:
        print(f"Map server running at http://localhost:{PORT}")
        httpd.serve_forever()
