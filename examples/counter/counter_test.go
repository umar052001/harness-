package counter

import "testing"

func TestCounter(t *testing.T) {
	var counter Counter
	if counter.Value()!=0{t.Fatal("must start at zero")}
	if err:=counter.Add(2);err!=nil{t.Fatal(err)}
	if err:=counter.Add(3);err!=nil{t.Fatal(err)}
	if counter.Value()!=5{t.Fatal("add failed")}
	if err:=counter.Add(-1);err==nil{t.Fatal("negative input accepted")}
	if counter.Value()!=5{t.Fatal("invalid input changed counter")}
}
