# cerber.ihatebot.com — reverse proxy to the cerber container (127.0.0.1:8080).
# Mirrors the very-nice-llm vhost: :80 is what Cloudflare (Flexible SSL) hits,
# :443 serves the same content for direct LAN https. Auth is decided per source
# IP by /etc/nginx/conf.d/cerber-maps.conf (LAN keyless, public key-required).
server {
    listen 80;
    listen [::]:80;
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name cerber.ihatebot.com;

    ssl_certificate     /etc/letsencrypt/live/cerber.ihatebot.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/cerber.ihatebot.com/privkey.pem;
    include             /etc/letsencrypt/options-ssl-nginx.conf;
    ssl_dhparam         /etc/letsencrypt/ssl-dhparams.pem;

    access_log /var/log/nginx/cerber.ihatebot.com.access.log main;
    error_log  /var/log/nginx/cerber.ihatebot.com.error.log;

    location / {
        proxy_pass http://127.0.0.1:18080;
        proxy_http_version 1.1;

        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Authorization     $cerber_auth;   # LAN keyless / public key-required

        # streaming (SSE) friendly
        proxy_buffering         off;
        proxy_request_buffering off;
        proxy_read_timeout      1h;
        proxy_send_timeout      1h;
        proxy_connect_timeout   30s;
        chunked_transfer_encoding on;

        client_max_body_size 0;
    }
}
