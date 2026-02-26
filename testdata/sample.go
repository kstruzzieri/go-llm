package main

import "fmt"

func Hello(name string) string {
	return fmt.Sprintf("Hello, %s!", name)
}

func Add(a, b int) int {
	return a + b
}

func Multiply(a, b int) int {
	return a * b
}

func main() {
	fmt.Println(Hello("World"))
	fmt.Println(Add(2, 3))
	fmt.Println(Multiply(4, 5))
}
