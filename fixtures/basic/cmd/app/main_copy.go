package main

import (
	"fmt"
	"os"
)

func main() {
	password := "hunter2hunter2xyz"
	envName := "APP_PASSWORD"
	fmt.Println(len(password), envName)
	if len(os.Args) > 1 {
		fmt.Printf("args: %v\n", os.Args[1:])
	}
}
