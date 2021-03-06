drop table if exists onlineddl_test;
create table onlineddl_test (
  id int auto_increment,
  g geometry,
  pt point,
  primary key(id)
) auto_increment=1;

drop event if exists onlineddl_test;
delimiter ;;
create event onlineddl_test
  on schedule every 1 second
  starts current_timestamp
  ends current_timestamp + interval 60 second
  on completion not preserve
  enable
  do
begin
  insert into onlineddl_test values (null, ST_GeomFromText('POINT(1 1)'), POINT(10,10));
  insert into onlineddl_test values (null, ST_GeomFromText('POINT(2 2)'), POINT(20,20));
  insert into onlineddl_test values (null, ST_GeomFromText('POINT(3 3)'), POINT(30,30));
end ;;
