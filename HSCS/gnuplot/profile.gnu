#!/usr/bin/env gnuplot

set term postscript eps enhanced color font 'Helvetica,40' linewidth 2 size 10,6
set output '../figure/profile.eps'
set style data histograms

set border -1 front lt black linewidth 1.000 dashtype solid
set boxwidth 0.5 absolute
set style fill   solid 1.00 border lt -1
#set grid nopolar
#set grid noxtics nomxtics ytics nomytics noztics nomztics \
# nox2tics nomx2tics noy2tics nomy2tics nocbtics nomcbtics
#set grid layerdefault   lt 0 linewidth 0.500,  lt 0 linewidth 0.500
set key title "" center
set key inside right top vertical
set key font ",32" width 3
set style histogram columnstacked title textcolor lt -1

set xtics border in scale 0,0 nomirror rotate by -45 autojustify
set style fill solid 1.00 border lt -1

set xlabel "Workloads (threads, resolution)"
set ylabel "Cause of VM-exit (million)"
#set format y '%2.0f%%'

set yrange [0:1.4]

plot '../data/profile.dat' using ($2/1000000):key(1) ti col, \
                        '' using ($3/1000000) ti col, \
                        '' using ($4/1000000) ti col, \
                        '' using ($5/1000000) ti col, \
                        '' using ($6/1000000) ti col, \
                        '' using ($7/1000000) ti col
