package up

import "net/http"

func ExampleGetCalculatedChecksum() {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		check, err := GetCalculatedChecksum("checksum")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write(check)
	})
}
