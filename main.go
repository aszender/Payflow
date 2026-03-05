package main

import (
	"fmt"
	"github.com/aszender/payflow/learn"
)

func main() {
	fmt.Println("Hello, World!")
	const name string = "Andres"

	var age int = 30
	city := "New York" // Type inference
	var message string
	message = fmt.Sprintf("I am name %d years old and I live in %s.", age, city)
	fmt.Println(message)

	learn.Functions()
	learn.StartHTTPServer()

}