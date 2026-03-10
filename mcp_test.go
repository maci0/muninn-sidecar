package main

import (
	"encoding/json"
	"fmt"
)

func main() {
	m := map[string]any{
		"tags": []string{"a", "b", "c"},
	}
	b, _ := json.Marshal(m)
	fmt.Println(string(b))
}
