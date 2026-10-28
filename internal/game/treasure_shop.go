package game

type TreasureShopItem struct {
	ID    string
	Name  string
	Price int
}

var TreasureShopItems = []TreasureShopItem{
	{ID: "1", Name: "回气丹", Price: 120},
	{ID: "2", Name: "青灵养气丹", Price: 350},
	{ID: "3", Name: "万载灵髓", Price: 1000},
	{ID: "4", Name: "凝元丹", Price: 100},
	{ID: "5", Name: "三纹清灵丹", Price: 200},
	{ID: "6", Name: "斗灵丹", Price: 500},
	{ID: "7", Name: "皇极丹", Price: 1000},
}
