package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"mime"
	"net/http"
	"time"
)

func (b *bridge) onvif(w http.ResponseWriter, r *http.Request, origin, localOrigin string) {
	if origin != localOrigin {
		http.Error(w, "请在本机控制台添加 ONVIF 摄像头", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 {
		http.Error(w, "配对码无效", http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	var result any
	switch r.URL.Path {
	case "/api/onvif/discover":
		items, err := discoverONVIF(ctx)
		if err != nil {
			http.Error(w, "ONVIF 搜索失败，请直接填写设备地址", http.StatusBadGateway)
			return
		}
		result = items
	case "/api/onvif/profiles":
		var form struct {
			Endpoint string
			Username string
			Password string
		}
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
		decoder.DisallowUnknownFields()
		if mediaType != "application/json" || decoder.Decode(&form) != nil || len(form.Endpoint) > 2048 ||
			len(form.Username) > 256 || len(form.Password) > 512 {
			http.Error(w, "设备地址或凭据格式无效", http.StatusBadRequest)
			return
		}
		camera, err := newONVIF(form.Endpoint, form.Username, form.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		inventory, err := camera.profiles(ctx)
		camera.client.CloseIdleConnections()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		result = inventory
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
