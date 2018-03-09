#!/usr/bin/env gnuplot

set term postscript eps enhanced color font 'Helvetica,22' linewidth 2
set output '../figure/fts_part.eps'
# set style histogram cluster gap 1
#set key inside right top vertical
unset key
set yrange [0:512]
set xrange [0:0.02]
#set format x "%.0f" 
#set xtics rotate by 45 out offset 0,-2.0
set ytics 128
set xtics 0.01
# set ytics mirror
set ylabel "PTE Index"
set xlabel "Time"
plot '../data/new_80000.dat' with points lt 7 pt 7 ps 1
