package main

import "fmt"

type Action interface {
	Run()
}

type oneAction struct{}

func (e *oneAction) Run() {
	fmt.Println("oneAction")
}

type twoAction struct{}

func (e *twoAction) Run() {
	fmt.Println("twoAction")
}

func executeAction(action Action) {
	action.Run()
}

func main() {
	oneAction := &oneAction{}
	twoAction := &twoAction{}
	executeAction(oneAction)
	executeAction(twoAction)
}
