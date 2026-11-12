package claudeteam

import (
	"context"
	"errors"
	"slices"
	"testing"
)

type memoryAdmin struct {
	seats []Seat
	writes []string
	lost bool
}
func(a *memoryAdmin) Members(context.Context)([]Seat,error){return slices.Clone(a.seats),nil}
func(a *memoryAdmin) SetTier(_ context.Context,id,tier string)error{
	a.writes=append(a.writes,id+":"+tier)
	for i:=range a.seats {if a.seats[i].AccountID==id {a.seats[i].SeatTier=tier}}
	if a.lost {return errors.New("reply lost")};return nil
}

func transactionFixture() (Transaction,*memoryAdmin,Pending,*[]string) {
	a:=&memoryAdmin{seats:[]Seat{{"a","team_tier_1"},{"b","unassigned"},{"outside","team_tier_1"}}}
	steps:=[]string{}
	p:=Pending{OrganizationID:"org",From:"a",To:"b",Baseline:[]string{"a","outside"}}
	tx:=Transaction{Config:Config{OrganizationID:"org",Members:[]string{"a","b"}},Admin:a,
	 Ready:func(string)error{steps=append(steps,"ready");return nil},
	 Close:func(string,string)error{steps=append(steps,"close");return nil},
	 Drain:func(context.Context,string)error{steps=append(steps,"drain");return nil},
	 Restore:func([]Seat)error{steps=append(steps,"restore");return nil},
	 Record:func(Pending)error{steps=append(steps,"record");return nil},
	 Complete:func(_ []Seat,_ Pending,result string)error{steps=append(steps,result);return nil},
	}
	return tx,a,p,&steps
}

func TestTransactionPreservesOrderingAndOutsideSeats(t *testing.T){
	tx,a,p,steps:=transactionFixture();a.lost=true
	if err:=tx.Transfer(context.Background(),a.seats,p);err!=nil{t.Fatal(err)}
	if !slices.Equal(*steps,[]string{"ready","close","drain","record","assigned"}) {t.Fatal(*steps)}
	if !slices.Equal(a.writes,[]string{"a:unassigned","b:team_tier_1"}) {t.Fatal(a.writes)}
	if a.seats[2].SeatTier!="team_tier_1"{t.Fatal("outside seat changed")}
}

func TestTransactionJournalFailureMakesNoCloudWrite(t *testing.T){
	tx,a,p,_:=transactionFixture();tx.Record=func(Pending)error{return errors.New("disk full")}
	if tx.Transfer(context.Background(),a.seats,p)==nil || len(a.writes)!=0{t.Fatal("unrecorded cloud mutation",a.writes)}
}

func TestTransactionDrainCancellationMakesNoCloudWrite(t *testing.T){
	tx,a,p,steps:=transactionFixture();tx.Drain=func(context.Context,string)error{return context.Canceled}
	if tx.Transfer(context.Background(),a.seats,p)==nil || len(a.writes)!=0{t.Fatal("changed a busy account",a.writes)}
	if !slices.Contains(*steps,"restore"){t.Fatal("admission was not restored")}
}

func TestRecoveryRejectsConcurrentOutsideChange(t *testing.T){
	tx,a,p,_:=transactionFixture();a.seats[0].SeatTier="unassigned";a.seats[2].SeatTier="unassigned"
	if tx.Recover(context.Background(),p)==nil || len(a.writes)!=0{t.Fatal("overwrote concurrent assignment",a.writes)}
}

func TestRecoveryRestoresOnlyAProvenFreeSeat(t *testing.T){
	tx,a,p,steps:=transactionFixture();a.seats[0].SeatTier="unassigned"
	if err:=tx.Recover(context.Background(),p);err!=nil{t.Fatal(err)}
	if !slices.Equal(a.writes,[]string{"a:team_tier_1"}) || !slices.Equal(*steps,[]string{"rolled_back"}){t.Fatal(a.writes,*steps)}
}
